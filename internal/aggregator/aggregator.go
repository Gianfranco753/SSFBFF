// Package aggregator fetches data from multiple upstream providers in parallel.
//
// It reads provider configuration (base URLs, timeouts, endpoint paths) and
// uses errgroup to fan out HTTP requests for each ProviderDep, then collects
// the responses into a map keyed by "provider.endpoint".
package aggregator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gcossani/ssfbff/runtime"
	"golang.org/x/sync/errgroup"
)

// EndpointConfig describes a single endpoint configuration with optional timeout override.
type EndpointConfig struct {
	Path    string        `yaml:"path"`
	Timeout time.Duration `yaml:"timeout"` // Optional, falls back to provider timeout
}

// UnmarshalYAML implements custom YAML unmarshaling to support both string and object formats.
// String format: "profile: /api/profile" -> EndpointConfig{Path: "/api/profile"}
// Object format: "profile: {path: /api/profile, timeout: 2s}" -> full config
func (e *EndpointConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var str string
	if err := unmarshal(&str); err == nil {
		e.Path = str
		return nil
	}

	var obj struct {
		Path    string        `yaml:"path"`
		Timeout time.Duration `yaml:"timeout"`
	}
	if err := unmarshal(&obj); err != nil {
		return err
	}
	e.Path = obj.Path
	e.Timeout = obj.Timeout
	return nil
}

// ProviderConfig describes a single upstream service. When Optional is true,
// a fetch failure stores null instead of stopping the request — this supports
// graceful degradation for non-critical services.
type ProviderConfig struct {
	BaseURL              string                      `yaml:"base_url"`
	Timeout              time.Duration               `yaml:"timeout"` // Provider-level default
	Endpoints            map[string]EndpointConfig   `yaml:"endpoints"` // name -> endpoint config
	Optional             bool                        `yaml:"optional"`
	MaxIdleConnsPerHost  int                         `yaml:"max_idle_conns_per_host"` // Optional, overrides env default
	MaxConnsPerHost      int                         `yaml:"max_conns_per_host"`     // Optional, overrides env default
}

// Aggregator holds provider configs and per-provider HTTP clients. It is safe for
// concurrent use and should be created once at startup.
type Aggregator struct {
	providers map[string]ProviderConfig
	clients   map[string]*http.Client // Per-provider clients with isolated connection pools
}

// LookupEnv is the function used to read environment variables during New().
// Tests can replace it to avoid depending on real env vars.
var LookupEnv = os.LookupEnv

// New creates an Aggregator from a provider config map and a client factory function.
// The factory function is called for each provider to create a dedicated HTTP client
// with its own connection pool. It resolves UPSTREAM_<PROVIDER>_URL environment overrides
// once at startup so that per-request lookups are just map reads with zero allocation.
func New(providers map[string]ProviderConfig, createClient func(ProviderConfig) *http.Client) *Aggregator {
	resolved := make(map[string]ProviderConfig, len(providers))
	clients := make(map[string]*http.Client, len(providers))

	for name, prov := range providers {
		envKey := "UPSTREAM_" + strings.ToUpper(name) + "_URL"
		if override, ok := LookupEnv(envKey); ok && override != "" {
			prov.BaseURL = override
		}
		// Normalize: strip trailing slash so endpoint paths join cleanly.
		prov.BaseURL = strings.TrimRight(prov.BaseURL, "/")
		// Apply default timeout once.
		if prov.Timeout == 0 {
			prov.Timeout = 10 * time.Second
		}
		resolved[name] = prov
		clients[name] = createClient(prov)
	}

	return &Aggregator{
		providers: resolved,
		clients:   clients,
	}
}

// Fetch calls all endpoints listed in deps concurrently and returns their raw
// JSON bodies keyed by "provider.endpoint". Each call respects the provider's
// configured timeout.
func (a *Aggregator) Fetch(ctx context.Context, deps []runtime.ProviderDep) (map[string][]byte, error) {
	results := make(map[string][]byte, len(deps))
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)

	for _, dep := range deps {
		g.Go(func() error {
			url, timeout, optional, err := a.resolveURL(dep)
			if err != nil {
				if optional {
					mu.Lock()
					results[dep.Key()] = []byte("null")
					mu.Unlock()
					return nil
				}
				return err
			}

			reqCtx, cancel := context.WithTimeout(gctx, timeout)
			defer cancel()

			body, err := a.doRequest(reqCtx, dep, url)
			if err != nil {
				if optional {
					mu.Lock()
					results[dep.Key()] = []byte("null")
					mu.Unlock()
					return nil
				}
				return fmt.Errorf("%s/%s: %w", dep.Provider, dep.Endpoint, err)
			}

			mu.Lock()
			results[dep.Key()] = body
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// resolveURL builds the full URL for a dep and returns the endpoint-specific timeout
// and whether the provider is optional. Timeout precedence: endpoint > provider > global default.
// Env overrides and default timeouts were already applied at startup in New().
func (a *Aggregator) resolveURL(dep runtime.ProviderDep) (url string, timeout time.Duration, optional bool, err error) {
	prov, ok := a.providers[dep.Provider]
	if !ok {
		return "", 0, false, fmt.Errorf("unknown provider %q", dep.Provider)
	}

	endpointCfg, ok := prov.Endpoints[dep.Endpoint]
	if !ok {
		return "", 0, prov.Optional, fmt.Errorf("provider %q has no endpoint %q", dep.Provider, dep.Endpoint)
	}

	// Timeout precedence: endpoint-specific > provider-level > global default (10s)
	timeout = endpointCfg.Timeout
	if timeout == 0 {
		timeout = prov.Timeout
	}
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	return prov.BaseURL + endpointCfg.Path, timeout, prov.Optional, nil
}

// doRequest makes an HTTP request respecting dep.Method, dep.Headers, and
// dep.Body. If Method is empty it defaults to GET. Uses the provider-specific client.
func (a *Aggregator) doRequest(ctx context.Context, dep runtime.ProviderDep, url string) ([]byte, error) {
	client, ok := a.clients[dep.Provider]
	if !ok {
		return nil, fmt.Errorf("no client configured for provider %q", dep.Provider)
	}

	method := dep.Method
	if method == "" {
		method = http.MethodGet
	}

	var bodyReader io.Reader
	if len(dep.Body) > 0 {
		bodyReader = bytes.NewReader(dep.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	for k, v := range dep.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
