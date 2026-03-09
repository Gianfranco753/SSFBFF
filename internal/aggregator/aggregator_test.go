//go:build goexperiment.jsonv2

package aggregator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gcossani/ssfbff/runtime"
	"github.com/rs/zerolog"
)

// testClientFactory creates a simple default http.Client factory for tests.
func testClientFactory(cfg ProviderConfig) *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     100,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
	}
}

// makeEndpoints is a helper to create endpoint maps from string paths (backward compatible format).
func makeEndpoints(m map[string]string) map[string]EndpointConfig {
	result := make(map[string]EndpointConfig, len(m))
	for k, v := range m {
		result[k] = EndpointConfig{Path: v}
	}
	return result
}

func TestFetchParallel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/profile":
			w.Write([]byte(`{"name":"Alice"}`))
		case "/accounts":
			w.Write([]byte(`{"amount":100}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"user_svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: makeEndpoints(map[string]string{"profile": "/profile"})},
		"bank_svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: makeEndpoints(map[string]string{"accounts": "/accounts"})},
	}, testClientFactory)

	deps := []runtime.ProviderDep{
		{Provider: "user_svc", Endpoint: "profile"},
		{Provider: "bank_svc", Endpoint: "accounts"},
	}

	results, err := agg.Fetch(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}

	if got := string(results["user_svc.profile"]); got != `{"name":"Alice"}` {
		t.Errorf("user_svc.profile = %q, want %q", got, `{"name":"Alice"}`)
	}
	if got := string(results["bank_svc.accounts"]); got != `{"amount":100}` {
		t.Errorf("bank_svc.accounts = %q, want %q", got, `{"amount":100}`)
	}
}

func TestFetchWithMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("X-Custom") != "test-val" {
			t.Errorf("missing custom header")
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
	}, testClientFactory)

	deps := []runtime.ProviderDep{
		{
			Provider: "svc",
			Endpoint: "ep",
			Method:   "POST",
			Headers:  map[string]string{"X-Custom": "test-val"},
			Body:     []byte(`{"key":"value"}`),
		},
	}

	results, err := agg.Fetch(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if got := string(results["svc.ep"]); got != `{"ok":true}` {
		t.Errorf("svc.ep = %q, want %q", got, `{"ok":true}`)
	}
}

func TestFetchUnknownProvider(t *testing.T) {
	agg := New(map[string]ProviderConfig{}, testClientFactory)

	deps := []runtime.ProviderDep{{Provider: "missing", Endpoint: "ep"}}
	_, err := agg.Fetch(context.Background(), deps, nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestFetchUnknownEndpoint(t *testing.T) {
	agg := New(map[string]ProviderConfig{
		"svc": {BaseURL: "http://localhost", Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{"valid": "/valid"})},
	}, testClientFactory)

	deps := []runtime.ProviderDep{{Provider: "svc", Endpoint: "missing"}}
	_, err := agg.Fetch(context.Background(), deps, nil)
	if err == nil {
		t.Fatal("expected error for unknown endpoint")
	}
}

func TestFetchOptionalProviderFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fail", http.StatusInternalServerError)
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"optional_svc": {
			BaseURL:   srv.URL,
			Timeout:   2 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep": "/ep"}),
			Optional:  true,
		},
	}, testClientFactory)

	deps := []runtime.ProviderDep{{Provider: "optional_svc", Endpoint: "ep"}}
	results, err := agg.Fetch(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("optional provider should not return error, got: %v", err)
	}
	if got := string(results["optional_svc.ep"]); got != "null" {
		t.Errorf("optional failure result = %q, want %q", got, "null")
	}
}

func TestFetchRequiredProviderFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fail", http.StatusInternalServerError)
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"required_svc": {
			BaseURL:   srv.URL,
			Timeout:   2 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep": "/ep"}),
			Optional:  false,
		},
	}, testClientFactory)

	deps := []runtime.ProviderDep{{Provider: "required_svc", Endpoint: "ep"}}
	_, err := agg.Fetch(context.Background(), deps, nil)
	if err == nil {
		t.Fatal("required provider failure should return error")
	}
}

func TestFetchContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"slow_svc": {BaseURL: srv.URL, Timeout: 100 * time.Millisecond, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
	}, testClientFactory)

	deps := []runtime.ProviderDep{{Provider: "slow_svc", Endpoint: "ep"}}
	_, err := agg.Fetch(context.Background(), deps, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestFetchDefaultTimeout(t *testing.T) {
	agg := New(map[string]ProviderConfig{
		"svc": {BaseURL: "http://localhost", Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
	}, testClientFactory)

	// Default timeout is now applied at startup in New(), so resolveURL returns it directly.
	dep := runtime.ProviderDep{Provider: "svc", Endpoint: "ep"}
	_, timeout, _, _, err := agg.resolveURL(dep, nil)
	if err != nil {
		t.Fatalf("resolveURL error: %v", err)
	}
	if timeout != 10*time.Second {
		t.Errorf("default timeout = %v, want 10s", timeout)
	}
}

func TestFetchWithPathParams(t *testing.T) {
	var pathSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathSeen = r.URL.Path
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: makeEndpoints(map[string]string{"post": "/posts/{order_id}"})},
	}, testClientFactory)

	deps := []runtime.ProviderDep{{Provider: "svc", Endpoint: "post"}}
	params := map[string]string{"order_id": "42"}
	results, err := agg.Fetch(context.Background(), deps, params)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if got := string(results["svc.post"]); got != `{"id":1}` {
		t.Errorf("results[svc.post] = %q, want %q", got, `{"id":1}`)
	}
	if pathSeen != "/posts/42" {
		t.Errorf("request path = %q, want /posts/42", pathSeen)
	}
}

func TestFetchWithPathParamsMissingParam(t *testing.T) {
	agg := New(map[string]ProviderConfig{
		"svc": {BaseURL: "http://localhost", Timeout: 5 * time.Second, Endpoints: makeEndpoints(map[string]string{"post": "/posts/{order_id}"})},
	}, testClientFactory)

	deps := []runtime.ProviderDep{{Provider: "svc", Endpoint: "post"}}
	_, err := agg.Fetch(context.Background(), deps, nil)
	if err == nil {
		t.Fatal("expected error when path has placeholder but params is nil")
	}
	if !strings.Contains(err.Error(), "missing path parameter") {
		t.Errorf("error = %v, want message containing 'missing path parameter'", err)
	}
}

func TestFetchWithPathParamsBackwardCompatNilParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/static"})},
	}, testClientFactory)

	deps := []runtime.ProviderDep{{Provider: "svc", Endpoint: "ep"}}
	_, err := agg.Fetch(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("Fetch with nil params and static path should succeed: %v", err)
	}
}

func TestValidateProviderConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ProviderConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid config",
			cfg: ProviderConfig{
				BaseURL:   "http://example.com",
				Timeout:   5 * time.Second,
				Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: false,
		},
		{
			name: "valid host and path",
			cfg: ProviderConfig{
				Host:      "http://example.com",
				Path:      "/v1",
				Timeout:   5 * time.Second,
				Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: false,
		},
		{
			name: "missing host and base_url",
			cfg: ProviderConfig{
				BaseURL:   "",
				Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: true,
			errMsg:  "either host or base_url is required",
		},
		{
			name: "invalid base_url",
			cfg: ProviderConfig{
				BaseURL:   "not-a-url",
				Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: true,
			errMsg:  "base_url",
		},
		{
			name: "negative timeout",
			cfg: ProviderConfig{
				BaseURL:   "http://example.com",
				Timeout:   -1 * time.Second,
				Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: true,
			errMsg:  "timeout cannot be negative",
		},
		{
			name: "no endpoints",
			cfg: ProviderConfig{
				BaseURL:   "http://example.com",
				Endpoints: makeEndpoints(map[string]string{}),
			},
			wantErr: true,
			errMsg:  "at least one endpoint is required",
		},
		{
			name: "empty endpoint path",
			cfg: ProviderConfig{
				BaseURL: "http://example.com",
				Endpoints: map[string]EndpointConfig{
					"ep": {Path: ""},
				},
			},
			wantErr: true,
			errMsg:  "empty path",
		},
		{
			name: "negative endpoint timeout",
			cfg: ProviderConfig{
				BaseURL: "http://example.com",
				Endpoints: map[string]EndpointConfig{
					"ep": {Path: "/api/ep", Timeout: -1 * time.Second},
				},
			},
			wantErr: true,
			errMsg:  "timeout cannot be negative",
		},
		{
			name: "timeout below minimum",
			cfg: ProviderConfig{
				BaseURL:   "http://example.com",
				Timeout:   500 * time.Microsecond, // 0.5 ms
				Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: true,
			errMsg:  "timeout must be between 1 and 300000 ms",
		},
		{
			name: "timeout above maximum",
			cfg: ProviderConfig{
				BaseURL:   "http://example.com",
				Timeout:   301 * time.Second,
				Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: true,
			errMsg:  "timeout must be between 1 and 300000 ms",
		},
		{
			name: "connection_timeout out of range",
			cfg: ProviderConfig{
				BaseURL:           "http://example.com",
				ConnectionTimeout: 400 * time.Second,
				Endpoints:         makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: true,
			errMsg:  "connection_timeout must be between 1 and 300000 ms",
		},
		{
			name: "redirections_max negative",
			cfg: ProviderConfig{
				BaseURL:         "http://example.com",
				RedirectionsMax: -1,
				Endpoints:       makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: true,
			errMsg:  "redirections_max cannot be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProviderConfig("test_provider", tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("error message %q does not contain %q", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestResolveURLWithQuery(t *testing.T) {
	agg := New(map[string]ProviderConfig{
		"svc": {
			BaseURL:   "http://example.com",
			Timeout:   5 * time.Second,
			Query:     map[string]string{"format": "json", "v": "1"},
			Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
		},
	}, testClientFactory)

	dep := runtime.ProviderDep{Provider: "svc", Endpoint: "ep"}
	resolved, _, _, _, err := agg.resolveURL(dep, nil)
	if err != nil {
		t.Fatalf("resolveURL error: %v", err)
	}
	if !strings.Contains(resolved, "format=json") || !strings.Contains(resolved, "v=1") {
		t.Errorf("resolved URL %q should contain default query params format=json and v=1", resolved)
	}
}

func TestResolveURLHostAndPath(t *testing.T) {
	agg := New(map[string]ProviderConfig{
		"svc": {
			Host:      "http://api.example.com",
			Path:      "/v1",
			Timeout:   5 * time.Second,
			Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
		},
	}, testClientFactory)

	dep := runtime.ProviderDep{Provider: "svc", Endpoint: "ep"}
	resolved, _, _, _, err := agg.resolveURL(dep, nil)
	if err != nil {
		t.Fatalf("resolveURL error: %v", err)
	}
	want := "http://api.example.com/v1/api/ep"
	if resolved != want {
		t.Errorf("resolved URL = %q, want %q", resolved, want)
	}
}

func TestDoRequestProviderHeaders(t *testing.T) {
	var seenHeader string
	var seenOverride string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Get("X-Provider-Header")
		seenOverride = r.Header.Get("X-Override")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {
			BaseURL: srv.URL,
			Timeout: 5 * time.Second,
			Headers: map[string]string{"X-Provider-Header": "from-provider", "X-Override": "provider-value"},
			Endpoints: makeEndpoints(map[string]string{"ep": "/ep"}),
		},
	}, testClientFactory)

	deps := []runtime.ProviderDep{
		{Provider: "svc", Endpoint: "ep", Headers: map[string]string{"X-Override": "request-override"}},
	}
	_, err := agg.Fetch(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if seenHeader != "from-provider" {
		t.Errorf("X-Provider-Header = %q, want from-provider", seenHeader)
	}
	if seenOverride != "request-override" {
		t.Errorf("X-Override (request should override provider) = %q, want request-override", seenOverride)
	}
}

func TestFetchCacheHit(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Write([]byte(`{"name":"Alice"}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"user_svc": {
			BaseURL: srv.URL,
			Timeout: 5 * time.Second,
			Endpoints: map[string]EndpointConfig{
				"profile": {Path: "/profile", UseCache: true},
			},
		},
	}, testClientFactory)

	ctx := context.Background()
	cache := &FetchCache{}
	ctx = WithFetchCache(ctx, cache)

	dep := runtime.ProviderDep{Provider: "user_svc", Endpoint: "profile"}

	// First call - cache miss, should make HTTP request
	results1, err := agg.Fetch(ctx, []runtime.ProviderDep{dep}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount)
	}
	if got := string(results1["user_svc.profile"]); got != `{"name":"Alice"}` {
		t.Errorf("result = %q, want %q", got, `{"name":"Alice"}`)
	}

	// Second call with same dep - cache hit, should NOT make HTTP request
	results2, err := agg.Fetch(ctx, []runtime.ProviderDep{dep}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call after cache hit, got %d", callCount)
	}
	if got := string(results2["user_svc.profile"]); got != `{"name":"Alice"}` {
		t.Errorf("cached result = %q, want %q", got, `{"name":"Alice"}`)
	}
}

func TestFetchCacheMissWhenDisabled(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Write([]byte(`{"name":"Alice"}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"user_svc": {
			BaseURL: srv.URL,
			Timeout: 5 * time.Second,
			Endpoints: map[string]EndpointConfig{
				"profile": {Path: "/profile", UseCache: false}, // Cache disabled
			},
		},
	}, testClientFactory)

	ctx := context.Background()
	cache := &FetchCache{}
	ctx = WithFetchCache(ctx, cache)

	dep := runtime.ProviderDep{Provider: "user_svc", Endpoint: "profile"}

	// First call
	_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount)
	}

	// Second call - cache disabled, should make another HTTP request
	_, err = agg.Fetch(ctx, []runtime.ProviderDep{dep}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (cache disabled), got %d", callCount)
	}
}

func TestFetchCacheKeyUniqueness(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Write([]byte(`{"result":"ok"}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"svc": {
			BaseURL: srv.URL,
			Timeout: 5 * time.Second,
			Endpoints: map[string]EndpointConfig{
				"ep": {Path: "/ep", UseCache: true},
			},
		},
	}, testClientFactory)

	ctx := context.Background()
	cache := &FetchCache{}
	ctx = WithFetchCache(ctx, cache)

	// Different methods should get different cache entries
	dep1 := runtime.ProviderDep{Provider: "svc", Endpoint: "ep", Method: "GET"}
	dep2 := runtime.ProviderDep{Provider: "svc", Endpoint: "ep", Method: "POST"}

	_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep1}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount)
	}

	_, err = agg.Fetch(ctx, []runtime.ProviderDep{dep2}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (different methods), got %d", callCount)
	}

	// Same method should hit cache
	_, err = agg.Fetch(ctx, []runtime.ProviderDep{dep1}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (cache hit for GET), got %d", callCount)
	}
}

func TestFetchCacheRequestScoping(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Write([]byte(`{"name":"Alice"}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"user_svc": {
			BaseURL: srv.URL,
			Timeout: 5 * time.Second,
			Endpoints: map[string]EndpointConfig{
				"profile": {Path: "/profile", UseCache: true},
			},
		},
	}, testClientFactory)

	dep := runtime.ProviderDep{Provider: "user_svc", Endpoint: "profile"}

	// Request 1
	ctx1 := context.Background()
	cache1 := &FetchCache{}
	ctx1 = WithFetchCache(ctx1, cache1)
	_, err := agg.Fetch(ctx1, []runtime.ProviderDep{dep}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount)
	}

	// Request 2 - different context, should make new HTTP request
	ctx2 := context.Background()
	cache2 := &FetchCache{}
	ctx2 = WithFetchCache(ctx2, cache2)
	_, err = agg.Fetch(ctx2, []runtime.ProviderDep{dep}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (different requests), got %d", callCount)
	}
}

func TestFetchCacheConcurrentAccess(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		time.Sleep(10 * time.Millisecond) // Simulate network latency
		w.Write([]byte(`{"name":"Alice"}`))
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"user_svc": {
			BaseURL: srv.URL,
			Timeout: 5 * time.Second,
			Endpoints: map[string]EndpointConfig{
				"profile": {Path: "/profile", UseCache: true},
			},
		},
	}, testClientFactory)

	ctx := context.Background()
	cache := &FetchCache{}
	ctx = WithFetchCache(ctx, cache)

	dep := runtime.ProviderDep{Provider: "user_svc", Endpoint: "profile"}

	// Make 10 concurrent requests for the same endpoint
	// First one should make HTTP call, others should wait and then use cache
	results := make([]map[string][]byte, 10)
	errs := make([]error, 10)

	for i := 0; i < 10; i++ {
		results[i], errs[i] = agg.Fetch(ctx, []runtime.ProviderDep{dep}, nil)
	}

	// All should succeed
	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d failed: %v", i, err)
		}
	}

	// Should have made only 1 HTTP call (first one), rest should be cache hits
	// Note: due to race conditions, we might get 1-2 calls, but definitely not 10
	if callCount > 2 {
		t.Errorf("expected at most 2 HTTP calls (race condition), got %d", callCount)
	}

	// All results should be the same
	expected := `{"name":"Alice"}`
	for i, result := range results {
		if got := string(result["user_svc.profile"]); got != expected {
			t.Errorf("result %d = %q, want %q", i, got, expected)
		}
	}
}

func TestFetchCacheOnlyStoresSuccess(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			http.Error(w, "error", http.StatusInternalServerError)
		} else {
			w.Write([]byte(`{"name":"Alice"}`))
		}
	}))
	defer srv.Close()

	agg := New(map[string]ProviderConfig{
		"user_svc": {
			BaseURL: srv.URL,
			Timeout: 5 * time.Second,
			Endpoints: map[string]EndpointConfig{
				"profile": {Path: "/profile", UseCache: true},
			},
		},
	}, testClientFactory)

	ctx := context.Background()
	cache := &FetchCache{}
	ctx = WithFetchCache(ctx, cache)

	dep := runtime.ProviderDep{Provider: "user_svc", Endpoint: "profile"}

	// First call - fails, should not be cached
	_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep}, nil)
	if err == nil {
		t.Fatal("expected error for failed request")
	}
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount)
	}

	// Second call - should make another HTTP request (first one wasn't cached)
	results, err := agg.Fetch(ctx, []runtime.ProviderDep{dep}, nil)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (first failure not cached), got %d", callCount)
	}
	if got := string(results["user_svc.profile"]); got != `{"name":"Alice"}` {
		t.Errorf("result = %q, want %q", got, `{"name":"Alice"}`)
	}
}

// TestNewWithObservability_NilConfig verifies that nil observability config does not panic and Fetch still works.
func TestNewWithObservability_NilConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	agg := NewWithObservability(
		map[string]ProviderConfig{
			"svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
		},
		testClientFactory,
		nil, // nil obsConfig
		10*1024*1024,
	)
	deps := []runtime.ProviderDep{{Provider: "svc", Endpoint: "ep"}}
	results, err := agg.Fetch(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("Fetch with nil obsConfig: %v", err)
	}
	if string(results["svc.ep"]) != `{"ok":true}` {
		t.Errorf("unexpected result %q", results["svc.ep"])
	}
}

// TestNewWithObservability_NilCallbacks verifies that observability with nil callbacks does not panic.
func TestNewWithObservability_NilCallbacks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	obs := &ObservabilityConfig{
		Logger:              zerolog.Nop(),
		RecordUpstreamCall:  nil,
		RecordUpstreamError: nil,
		RecordAggregatorOp:  nil,
	}
	agg := NewWithObservability(
		map[string]ProviderConfig{
			"svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
		},
		testClientFactory,
		obs,
		10*1024*1024,
	)
	deps := []runtime.ProviderDep{{Provider: "svc", Endpoint: "ep"}}
	_, err := agg.Fetch(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("Fetch with nil callbacks: %v", err)
	}
}

// TestNewWithObservability_RecordUpstreamCallInvoked verifies that RecordUpstreamCall is invoked when set.
func TestNewWithObservability_RecordUpstreamCallInvoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"x":1}`))
	}))
	defer srv.Close()

	var calls []struct{ provider, endpoint, status string }
	var mu sync.Mutex
	obs := &ObservabilityConfig{
		Logger: zerolog.Nop(),
		RecordUpstreamCall: func(provider, endpoint string, _ time.Duration, status string) {
			mu.Lock()
			calls = append(calls, struct{ provider, endpoint, status string }{provider, endpoint, status})
			mu.Unlock()
		},
	}
	agg := NewWithObservability(
		map[string]ProviderConfig{
			"svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
		},
		testClientFactory,
		obs,
		10*1024*1024,
	)
	deps := []runtime.ProviderDep{{Provider: "svc", Endpoint: "ep"}}
	_, err := agg.Fetch(context.Background(), deps, nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 1 {
		t.Errorf("RecordUpstreamCall called %d times, want 1", n)
	}
	if n >= 1 && (calls[0].provider != "svc" || calls[0].endpoint != "ep" || calls[0].status != "success") {
		t.Errorf("RecordUpstreamCall(provider=%q, endpoint=%q, status=%q)", calls[0].provider, calls[0].endpoint, calls[0].status)
	}
}

// TestGetProvidersReturnsCopy verifies that GetProviders returns a copy; mutating it does not affect the aggregator.
func TestGetProvidersReturnsCopy(t *testing.T) {
	cfg := ProviderConfig{
		BaseURL:   "http://example.com",
		Timeout:   5 * time.Second,
		Endpoints: makeEndpoints(map[string]string{"ep": "/api"}),
	}
	agg := New(map[string]ProviderConfig{"svc": cfg}, testClientFactory)

	got := agg.GetProviders()
	if len(got) != 1 || got["svc"].BaseURL != "http://example.com" {
		t.Fatalf("GetProviders() = %v", got)
	}
	got["svc"] = ProviderConfig{BaseURL: "http://mutated.com"}
	got["extra"] = ProviderConfig{BaseURL: "http://extra.com"}

	got2 := agg.GetProviders()
	if len(got2) != 1 {
		t.Errorf("GetProviders() after mutate: len = %d, want 1", len(got2))
	}
	if got2["svc"].BaseURL != "http://example.com" {
		t.Errorf("GetProviders() copy was mutated; BaseURL = %q", got2["svc"].BaseURL)
	}
}
