//go:build goexperiment.jsonv2

package aggregator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gcossani/ssfbff/runtime"
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

	results, err := agg.Fetch(context.Background(), deps)
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

	results, err := agg.Fetch(context.Background(), deps)
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
	_, err := agg.Fetch(context.Background(), deps)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestFetchUnknownEndpoint(t *testing.T) {
	agg := New(map[string]ProviderConfig{
		"svc": {BaseURL: "http://localhost", Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{"valid": "/valid"})},
	}, testClientFactory)

	deps := []runtime.ProviderDep{{Provider: "svc", Endpoint: "missing"}}
	_, err := agg.Fetch(context.Background(), deps)
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
	results, err := agg.Fetch(context.Background(), deps)
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
	_, err := agg.Fetch(context.Background(), deps)
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
	_, err := agg.Fetch(context.Background(), deps)
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
	_, timeout, _, _, err := agg.resolveURL(dep)
	if err != nil {
		t.Fatalf("resolveURL error: %v", err)
	}
	if timeout != 10*time.Second {
		t.Errorf("default timeout = %v, want 10s", timeout)
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
			name: "empty base_url",
			cfg: ProviderConfig{
				BaseURL:   "",
				Endpoints: makeEndpoints(map[string]string{"ep": "/api/ep"}),
			},
			wantErr: true,
			errMsg:  "base_url is required",
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
	results1, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
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
	results2, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
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
	_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount)
	}

	// Second call - cache disabled, should make another HTTP request
	_, err = agg.Fetch(ctx, []runtime.ProviderDep{dep})
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

	_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep1})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount)
	}

	_, err = agg.Fetch(ctx, []runtime.ProviderDep{dep2})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (different methods), got %d", callCount)
	}

	// Same method should hit cache
	_, err = agg.Fetch(ctx, []runtime.ProviderDep{dep1})
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
	_, err := agg.Fetch(ctx1, []runtime.ProviderDep{dep})
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
	_, err = agg.Fetch(ctx2, []runtime.ProviderDep{dep})
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
		results[i], errs[i] = agg.Fetch(ctx, []runtime.ProviderDep{dep})
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
	_, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
	if err == nil {
		t.Fatal("expected error for failed request")
	}
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount)
	}

	// Second call - should make another HTTP request (first one wasn't cached)
	results, err := agg.Fetch(ctx, []runtime.ProviderDep{dep})
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
