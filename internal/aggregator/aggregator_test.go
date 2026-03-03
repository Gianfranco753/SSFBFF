package aggregator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gcossani/ssfbff/runtime"
)

// testClientFactory creates a simple default http.Client factory for tests.
func testClientFactory(cfg ProviderConfig) *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
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
		"svc": {BaseURL: "http://localhost", Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{})},
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
	_, timeout, _, err := agg.resolveURL(dep)
	if err != nil {
		t.Fatalf("resolveURL error: %v", err)
	}
	if timeout != 10*time.Second {
		t.Errorf("default timeout = %v, want 10s", timeout)
	}
}
