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
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gcossani/ssfbff/runtime"
	"golang.org/x/sync/errgroup"
)

// ProviderConfig describes a single upstream service.
type ProviderConfig struct {
	BaseURL  string                    `yaml:"base_url"`
	Timeout  time.Duration             `yaml:"timeout"`
	Endpoints map[string]string        `yaml:"endpoints"` // name -> path
}

// Aggregator holds provider configs and an HTTP client pool. It is safe for
// concurrent use and should be created once at startup.
type Aggregator struct {
	providers map[string]ProviderConfig
	client    *http.Client
	bufPool   sync.Pool
}

// New creates an Aggregator from a provider config map.
func New(providers map[string]ProviderConfig) *Aggregator {
	return &Aggregator{
		providers: providers,
		client: &http.Client{
			// Per-request timeouts are applied via context; this is a safety net.
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   3 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
		bufPool: sync.Pool{
			New: func() any { return new(bytes.Buffer) },
		},
	}
}

// Fetch calls all endpoints listed in deps concurrently and returns their raw
// JSON bodies keyed by "provider.endpoint". Each call respects the provider's
// configured timeout. Base URLs can be overridden with UPSTREAM_<PROVIDER>_URL
// environment variables.
func (a *Aggregator) Fetch(ctx context.Context, deps []runtime.ProviderDep) (map[string][]byte, error) {
	results := make(map[string][]byte, len(deps))
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)

	for _, dep := range deps {
		g.Go(func() error {
			url, timeout, err := a.resolveURL(dep)
			if err != nil {
				return err
			}

			reqCtx, cancel := context.WithTimeout(gctx, timeout)
			defer cancel()

			body, err := a.doGet(reqCtx, url)
			if err != nil {
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

// resolveURL builds the full URL for a dep and returns the provider timeout.
// It checks for an env var override first: UPSTREAM_<PROVIDER>_URL.
func (a *Aggregator) resolveURL(dep runtime.ProviderDep) (string, time.Duration, error) {
	prov, ok := a.providers[dep.Provider]
	if !ok {
		return "", 0, fmt.Errorf("unknown provider %q", dep.Provider)
	}

	path, ok := prov.Endpoints[dep.Endpoint]
	if !ok {
		return "", 0, fmt.Errorf("provider %q has no endpoint %q", dep.Provider, dep.Endpoint)
	}

	// Allow env var override for the base URL (same convention as existing BFF).
	envKey := "UPSTREAM_" + strings.ToUpper(dep.Provider) + "_URL"
	baseURL := os.Getenv(envKey)
	if baseURL == "" {
		baseURL = prov.BaseURL
	}

	timeout := prov.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	url := strings.TrimRight(baseURL, "/") + path
	return url, timeout, nil
}

func (a *Aggregator) doGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s returned %d", url, resp.StatusCode)
	}

	buf := a.bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer a.bufPool.Put(buf)

	if _, err := io.Copy(buf, resp.Body); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}
