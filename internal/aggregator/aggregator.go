//go:build goexperiment.jsonv2

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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gcossani/ssfbff/runtime"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

// fetchCacheKey is a private type for context key to avoid collisions.
type fetchCacheKey struct{}

// FetchCache provides request-scoped caching for $fetch() responses.
// Uses sync.Map for lock-free concurrent access optimized for high throughput.
type FetchCache struct {
	m sync.Map // map[string][]byte - cache key -> response body
}

// WithFetchCache attaches a FetchCache to the context for request-scoped caching.
func WithFetchCache(ctx context.Context, cache *FetchCache) context.Context {
	return context.WithValue(ctx, fetchCacheKey{}, cache)
}

// FetchCacheFromContext extracts the FetchCache from context if present.
func FetchCacheFromContext(ctx context.Context) (*FetchCache, bool) {
	cache, ok := ctx.Value(fetchCacheKey{}).(*FetchCache)
	return cache, ok
}

// EndpointConfig describes a single endpoint configuration with optional timeout override.
type EndpointConfig struct {
	Path     string        `yaml:"path"`
	Timeout  time.Duration `yaml:"timeout"` // Optional, falls back to provider timeout
	UseCache bool          `yaml:"use_cache"` // Optional, enables request-scoped caching (default false)
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
		Path     string        `yaml:"path"`
		Timeout  time.Duration `yaml:"timeout"`
		UseCache bool          `yaml:"use_cache"`
	}
	if err := unmarshal(&obj); err != nil {
		return err
	}
	e.Path = obj.Path
	e.Timeout = obj.Timeout
	e.UseCache = obj.UseCache
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
	obsConfig *ObservabilityConfig   // Optional observability configuration
	MaxResponseBodySize int // Maximum response body size in bytes
	
	// Feature flags set at initialization to avoid nil checks in hot path
	hasObservability      bool
	hasLogger             bool
	hasRecordUpstreamCall bool
	hasRecordUpstreamError bool
	hasRecordAggregatorOp bool
}

// ValidateProviderConfig validates a provider configuration and returns an error if invalid.
// It checks that base_url is a valid URL, timeouts are non-negative, and at least one
// endpoint is defined with a non-empty path.
func ValidateProviderConfig(name string, cfg ProviderConfig) error {
	if cfg.BaseURL == "" {
		return fmt.Errorf("provider %q: base_url is required", name)
	}

	parsedURL, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return fmt.Errorf("provider %q: invalid base_url %q: %w", name, cfg.BaseURL, err)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return fmt.Errorf("provider %q: base_url %q must include scheme and host", name, cfg.BaseURL)
	}

	if cfg.Timeout < 0 {
		return fmt.Errorf("provider %q: timeout cannot be negative (got %v)", name, cfg.Timeout)
	}

	if len(cfg.Endpoints) == 0 {
		return fmt.Errorf("provider %q: at least one endpoint is required", name)
	}

	for endpointName, endpointCfg := range cfg.Endpoints {
		if endpointCfg.Path == "" {
			return fmt.Errorf("provider %q: endpoint %q has empty path", name, endpointName)
		}
		if endpointCfg.Timeout < 0 {
			return fmt.Errorf("provider %q: endpoint %q timeout cannot be negative (got %v)", name, endpointName, endpointCfg.Timeout)
		}
	}

	return nil
}

// New creates an Aggregator from a provider config map and a client factory function.
// The factory function is called for each provider to create a dedicated HTTP client
// with its own connection pool.
func New(providers map[string]ProviderConfig, createClient func(ProviderConfig) *http.Client) *Aggregator {
	return NewWithObservability(providers, createClient, nil, 10*1024*1024) // Default 10MB
}

// NewWithObservability creates an Aggregator with optional observability configuration.
// It validates all provider configurations before creating clients.
// maxResponseBodySize is the maximum size in bytes for response bodies (default 10MB if 0).
func NewWithObservability(providers map[string]ProviderConfig, createClient func(ProviderConfig) *http.Client, obsConfig *ObservabilityConfig, maxResponseBodySize int) *Aggregator {
	resolved := make(map[string]ProviderConfig, len(providers))
	clients := make(map[string]*http.Client, len(providers))

	for name, prov := range providers {
		// Validate configuration before processing
		if err := ValidateProviderConfig(name, prov); err != nil {
			// This should not happen if validation was done in loadProviders,
			// but we validate again here as a safety check.
			panic(fmt.Sprintf("invalid provider config for %q: %v", name, err))
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

	if maxResponseBodySize == 0 {
		maxResponseBodySize = 10 * 1024 * 1024 // Default 10MB
	}

	agg := &Aggregator{
		providers: resolved,
		clients:   clients,
		obsConfig: obsConfig,
		MaxResponseBodySize: maxResponseBodySize,
	}
	
	// Set feature flags once at initialization to avoid repeated nil checks
	if obsConfig != nil {
		agg.hasObservability = true
		agg.hasLogger = true // Logger is always set if obsConfig is provided
		agg.hasRecordUpstreamCall = obsConfig.RecordUpstreamCall != nil
		agg.hasRecordUpstreamError = obsConfig.RecordUpstreamError != nil
		agg.hasRecordAggregatorOp = obsConfig.RecordAggregatorOp != nil
	}
	
	return agg
}

// GetProviders returns a copy of the provider configurations.
// This is used for health checks and observability.
func (a *Aggregator) GetProviders() map[string]ProviderConfig {
	result := make(map[string]ProviderConfig, len(a.providers))
	for k, v := range a.providers {
		result[k] = v
	}
	return result
}

// Fetch calls all endpoints listed in deps concurrently and returns their raw
// JSON bodies keyed by "provider.endpoint". Each call respects the provider's
// configured timeout.
func (a *Aggregator) Fetch(ctx context.Context, deps []runtime.ProviderDep) (map[string][]byte, error) {
	startTime := time.Now()
	// Pre-allocate results map with all keys initialized to nil.
	// Each goroutine writes to a unique key (dep.Key() is unique per dependency),
	// but Go maps require synchronization for concurrent writes.
	results := make(map[string][]byte, len(deps))
	var resultsMu sync.Mutex
	for _, dep := range deps {
		results[dep.Key()] = nil // Initialize slot
	}

	g, gctx := errgroup.WithContext(ctx)

		for _, dep := range deps {
		dep := dep // capture loop variable
		g.Go(func() error {
			url, timeout, endpointCfg, optional, err := a.resolveURL(dep)
			if err != nil {
				a.logWithContext(gctx, zerolog.ErrorLevel, "failed to resolve upstream URL",
					func(e *zerolog.Event) {
						e.Str("provider", dep.Provider).
							Str("endpoint", dep.Endpoint).
							Err(err)
					})
				a.recordUpstreamError(dep.Provider, dep.Endpoint, "resolve_error")
				if optional {
					resultsMu.Lock()
					results[dep.Key()] = []byte("null")
					resultsMu.Unlock()
					return nil
				}
				return err
			}

			// Check cache if enabled (early return for zero overhead when disabled)
			if endpointCfg.UseCache {
				if cache, ok := FetchCacheFromContext(gctx); ok {
					cacheKey := dep.CacheKey()
					if cached, hit := cache.m.Load(cacheKey); hit {
						// Cache hit - use cached response
						resultsMu.Lock()
						results[dep.Key()] = cached.([]byte)
						resultsMu.Unlock()
						a.recordUpstreamCall(dep.Provider, dep.Endpoint, 0, "cache_hit")
						return nil
					}
				}
			}

			reqCtx, cancel := context.WithTimeout(gctx, timeout)
			defer cancel()

			callStart := time.Now()
			body, statusCode, err := a.doRequest(reqCtx, dep, url)
			callDuration := time.Since(callStart)
			
			if err != nil {
				status := "error"
				errorType := classifyUpstreamError(statusCode)
				
				requestMethod := dep.Method
				if requestMethod == "" {
					requestMethod = http.MethodGet
				}
				requestBodySize := len(dep.Body)
				sanitizedHeaders := sanitizeHeaders(dep.Headers)
				
				a.logWithContext(reqCtx, zerolog.ErrorLevel, "upstream request failed",
					func(e *zerolog.Event) {
						e.Str("provider", dep.Provider).
							Str("endpoint", dep.Endpoint).
							Str("url", url).
							Str("method", requestMethod).
							Int("request_body_size", requestBodySize).
							Dur("duration_ms", callDuration).
							Err(err)
						if statusCode > 0 {
							e.Int("status_code", statusCode)
						}
						if len(sanitizedHeaders) > 0 {
							e.Interface("headers", sanitizedHeaders)
						}
					})
				a.recordUpstreamError(dep.Provider, dep.Endpoint, errorType)
				a.recordUpstreamCall(dep.Provider, dep.Endpoint, callDuration, status)
				
				if optional {
					resultsMu.Lock()
					results[dep.Key()] = []byte("null")
					resultsMu.Unlock()
					a.logWithContext(reqCtx, zerolog.WarnLevel, "optional provider failed, using null",
						func(e *zerolog.Event) {
							e.Str("provider", dep.Provider).
								Str("endpoint", dep.Endpoint)
						})
					return nil
				}
				// Non-optional provider failed - return error
				return err
			}

			a.recordUpstreamCall(dep.Provider, dep.Endpoint, callDuration, "success")
			
			// Store in cache only on success and if enabled
			if err == nil && statusCode < 400 && endpointCfg.UseCache {
				if cache, ok := FetchCacheFromContext(gctx); ok {
					cache.m.Store(dep.CacheKey(), body)
				}
			}
			
			resultsMu.Lock()
			results[dep.Key()] = body
			resultsMu.Unlock()
			return nil
		})
	}

	err := g.Wait()
	totalDuration := time.Since(startTime)
	
	if err != nil {
		a.recordAggregatorOperation("failure")
		return nil, err
	}
	
	a.recordAggregatorOperation("success")
	if totalDuration > 1*time.Second {
		a.logWithContext(ctx, zerolog.WarnLevel, "slow aggregation detected",
			func(e *zerolog.Event) {
				e.Dur("total_duration_ms", totalDuration).
					Int("dependencies", len(deps))
			})
	}
	
	return results, nil
}

// resolveURL builds the full URL for a dep and returns the endpoint-specific timeout,
// endpoint config, and whether the provider is optional. Timeout precedence: endpoint > provider > global default.
// Default timeouts were already applied at startup in New().
func (a *Aggregator) resolveURL(dep runtime.ProviderDep) (url string, timeout time.Duration, endpointCfg EndpointConfig, optional bool, err error) {
	prov, ok := a.providers[dep.Provider]
	if !ok {
		// Sanitize error - don't expose provider name to client
		return "", 0, EndpointConfig{}, false, fmt.Errorf("unknown provider")
	}

	endpointCfg, ok = prov.Endpoints[dep.Endpoint]
	if !ok {
		// List available endpoints for logging (server-side only)
		available := make([]string, 0, len(prov.Endpoints))
		for k := range prov.Endpoints {
			available = append(available, k)
		}
		// Sanitize error - don't expose provider/endpoint names to client
		// Full details are logged server-side above
		return "", 0, EndpointConfig{}, prov.Optional, fmt.Errorf("endpoint not found")
	}

	// Timeout precedence: endpoint-specific > provider-level > global default (10s)
	timeout = endpointCfg.Timeout
	hasEndpointTimeout := timeout > 0
	if !hasEndpointTimeout {
		timeout = prov.Timeout
	}
	hasProviderTimeout := timeout > 0
	if !hasProviderTimeout {
		timeout = 10 * time.Second
	}

	return prov.BaseURL + endpointCfg.Path, timeout, endpointCfg, prov.Optional, nil
}

// classifyUpstreamError determines the error type based on status code.
func classifyUpstreamError(statusCode int) string {
	if statusCode >= 500 {
		return "server_error"
	}
	if statusCode == 408 || statusCode == 504 {
		return "timeout"
	}
	if statusCode >= 400 {
		return "client_error"
	}
	return "request_error"
}

// logWithContext logs a message using either LogFunc (if available) or Logger.
// This consolidates the duplicate logging pattern throughout the aggregator.
func (a *Aggregator) logWithContext(ctx context.Context, level zerolog.Level, msg string, fields func(*zerolog.Event)) {
	if !a.hasLogger {
		return
	}
	if a.obsConfig.LogFunc != nil {
		a.obsConfig.LogFunc(ctx, level, msg, fields)
	} else {
		var event *zerolog.Event
		switch level {
		case zerolog.DebugLevel:
			event = a.obsConfig.Logger.Debug()
		case zerolog.InfoLevel:
			event = a.obsConfig.Logger.Info()
		case zerolog.WarnLevel:
			event = a.obsConfig.Logger.Warn()
		case zerolog.ErrorLevel:
			event = a.obsConfig.Logger.Error()
		case zerolog.FatalLevel:
			event = a.obsConfig.Logger.Fatal()
		case zerolog.PanicLevel:
			event = a.obsConfig.Logger.Panic()
		default:
			event = a.obsConfig.Logger.Info()
		}
		fields(event)
		event.Msg(msg)
	}
}

// sanitizeHeaders removes sensitive headers from a map for safe logging.
// Headers like Authorization, X-Api-Key, etc. are replaced with "[redacted]".
func sanitizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	sanitized := make(map[string]string, len(headers))
	sensitiveHeaders := map[string]bool{
		"authorization": true,
		"x-api-key":     true,
		"x-auth-token":  true,
		"cookie":        true,
		"set-cookie":    true,
	}
	for k, v := range headers {
		keyLower := strings.ToLower(k)
		if sensitiveHeaders[keyLower] {
			sanitized[k] = "[redacted]"
		} else {
			sanitized[k] = v
		}
	}
	return sanitized
}

// doRequest makes an HTTP request respecting dep.Method, dep.Headers, and
// dep.Body. If Method is empty it defaults to GET. Uses the provider-specific client.
// Returns body, statusCode, error. statusCode is 0 if the request didn't reach the server.
func (a *Aggregator) doRequest(ctx context.Context, dep runtime.ProviderDep, url string) ([]byte, int, error) {
	client, ok := a.clients[dep.Provider]
	if !ok {
		// Sanitize error - don't expose provider name to client
		return nil, 0, fmt.Errorf("client not configured")
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
		// Sanitize error - remove URL and method details
		sanitizedErr := runtime.SanitizeError(err)
		return nil, 0, fmt.Errorf("failed to create request: %s", sanitizedErr)
	}

	for k, v := range dep.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		// Sanitize error - remove URL and method details
		sanitizedErr := runtime.SanitizeError(err)
		return nil, 0, fmt.Errorf("%s", sanitizedErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Sanitize error - remove URL and method details
		return nil, resp.StatusCode, fmt.Errorf("upstream service returned error status %d", resp.StatusCode)
	}

	limitedReader := io.LimitReader(resp.Body, int64(a.MaxResponseBodySize))
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		// Sanitize error - remove URL and method details
		sanitizedErr := runtime.SanitizeError(err)
		return nil, resp.StatusCode, fmt.Errorf("failed to read response: %s", sanitizedErr)
	}

	// Check for truncation by trying to read one more byte
	var extraByte [1]byte
	n, _ := resp.Body.Read(extraByte[:])
	if n > 0 {
		// Response was truncated - return error
		return nil, resp.StatusCode, fmt.Errorf("response body exceeds maximum size of %d bytes", a.MaxResponseBodySize)
	}
	
	return body, resp.StatusCode, nil
}
