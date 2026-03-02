package aggregator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gcossani/ssfbff/runtime"
)

// testClient creates a simple default http.Client for tests.
func testClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
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
		"user_svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: map[string]string{"profile": "/profile"}},
		"bank_svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: map[string]string{"accounts": "/accounts"}},
	}, testClient())

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
		"svc": {BaseURL: srv.URL, Timeout: 5 * time.Second, Endpoints: map[string]string{"ep": "/ep"}},
	}, testClient())

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
	agg := New(map[string]ProviderConfig{}, testClient())

	deps := []runtime.ProviderDep{{Provider: "missing", Endpoint: "ep"}}
	_, err := agg.Fetch(context.Background(), deps)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestFetchUnknownEndpoint(t *testing.T) {
	agg := New(map[string]ProviderConfig{
		"svc": {BaseURL: "http://localhost", Timeout: 1 * time.Second, Endpoints: map[string]string{}},
	}, testClient())

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
			Endpoints: map[string]string{"ep": "/ep"},
			Optional:  true,
		},
	}, testClient())

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
			Endpoints: map[string]string{"ep": "/ep"},
			Optional:  false,
		},
	}, testClient())

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
		"slow_svc": {BaseURL: srv.URL, Timeout: 100 * time.Millisecond, Endpoints: map[string]string{"ep": "/ep"}},
	}, testClient())

	deps := []runtime.ProviderDep{{Provider: "slow_svc", Endpoint: "ep"}}
	_, err := agg.Fetch(context.Background(), deps)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestFetchEnvOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"from":"env"}`))
	}))
	defer srv.Close()

	// Env overrides are now resolved at startup in New(). We set the env var
	// before calling New() so the override is applied.
	t.Setenv("UPSTREAM_MY_SVC_URL", srv.URL)

	agg := New(map[string]ProviderConfig{
		"my_svc": {
			BaseURL:   "http://should-not-be-used:1234",
			Timeout:   5 * time.Second,
			Endpoints: map[string]string{"ep": "/ep"},
		},
	}, testClient())

	deps := []runtime.ProviderDep{{Provider: "my_svc", Endpoint: "ep"}}
	results, err := agg.Fetch(context.Background(), deps)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if got := string(results["my_svc.ep"]); got != `{"from":"env"}` {
		t.Errorf("got %q, want %q", got, `{"from":"env"}`)
	}
}

func TestFetchDefaultTimeout(t *testing.T) {
	agg := New(map[string]ProviderConfig{
		"svc": {BaseURL: "http://localhost", Endpoints: map[string]string{"ep": "/ep"}},
	}, testClient())

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
