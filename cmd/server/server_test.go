//go:build goexperiment.jsonv2

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gcossani/ssfbff/internal/aggregator"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestLoadProviders(t *testing.T) {
	dir := t.TempDir()

	user := `base_url: http://user-svc:8080
timeout: 5s
endpoints:
  profile: /api/profile
`
	bank := `base_url: http://bank-svc:8080
timeout: 3s
optional: true
endpoints:
  accounts: /api/accounts
`
	os.WriteFile(filepath.Join(dir, "user_service.yaml"), []byte(user), 0o644)
	os.WriteFile(filepath.Join(dir, "bank_service.yaml"), []byte(bank), 0o644)
	// Non-YAML file should be skipped.
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignore me"), 0o644)

	providers, err := loadProviders(dir)
	if err != nil {
		t.Fatalf("loadProviders error: %v", err)
	}

	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}

	userCfg, ok := providers["user_service"]
	if !ok {
		t.Fatal("missing user_service provider")
	}
	if userCfg.BaseURL != "http://user-svc:8080" {
		t.Errorf("user_service base_url = %q", userCfg.BaseURL)
	}
	profileEndpoint, ok := userCfg.Endpoints["profile"]
	if !ok {
		t.Error("user_service missing profile endpoint")
	} else if profileEndpoint.Path != "/api/profile" {
		t.Errorf("user_service profile endpoint path = %q, want /api/profile", profileEndpoint.Path)
	}
	if userCfg.Optional {
		t.Error("user_service should not be optional")
	}

	bankCfg := providers["bank_service"]
	if !bankCfg.Optional {
		t.Error("bank_service should be optional")
	}
}

func TestLoadProvidersWithNewOptions(t *testing.T) {
	dir := t.TempDir()

	yamlWithNewOptions := `host: https://api.example.com
path: /v1
timeout: 5s
connection_timeout: 500ms
redirections_max: 3
headers:
  X-Api-Version: "1"
  Accept: application/json
query:
  format: json
endpoints:
  ep: /api/ep
`
	os.WriteFile(filepath.Join(dir, "api_service.yaml"), []byte(yamlWithNewOptions), 0o644)

	providers, err := loadProviders(dir)
	if err != nil {
		t.Fatalf("loadProviders error: %v", err)
	}

	cfg, ok := providers["api_service"]
	if !ok {
		t.Fatal("missing api_service provider")
	}
	if cfg.Host != "https://api.example.com" {
		t.Errorf("host = %q", cfg.Host)
	}
	if cfg.Path != "/v1" {
		t.Errorf("path = %q", cfg.Path)
	}
	if cfg.ConnectionTimeout != 500*time.Millisecond {
		t.Errorf("connection_timeout = %v", cfg.ConnectionTimeout)
	}
	if cfg.RedirectionsMax != 3 {
		t.Errorf("redirections_max = %d", cfg.RedirectionsMax)
	}
	if cfg.Headers["X-Api-Version"] != "1" || cfg.Headers["Accept"] != "application/json" {
		t.Errorf("headers = %v", cfg.Headers)
	}
	if cfg.Query["format"] != "json" {
		t.Errorf("query = %v", cfg.Query)
	}
}

func TestLoadProvidersInvalidConfig(t *testing.T) {
	dir := t.TempDir()

	// Missing both host and base_url
	os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(`
endpoints:
  ep: /ep
`), 0o644)
	_, err := loadProviders(dir)
	if err == nil {
		t.Fatal("expected error when both host and base_url are missing")
	}
	if !strings.Contains(err.Error(), "either host or base_url is required") {
		t.Errorf("error = %v", err)
	}

	// Timeout out of range
	os.Remove(filepath.Join(dir, "bad.yaml"))
	os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(`
base_url: http://example.com
timeout: 400s
endpoints:
  ep: /ep
`), 0o644)
	_, err = loadProviders(dir)
	if err == nil {
		t.Fatal("expected error when timeout exceeds 300s")
	}
	if !strings.Contains(err.Error(), "timeout must be between") {
		t.Errorf("error = %v", err)
	}
}

func TestLoadProvidersEmptyDir(t *testing.T) {
	dir := t.TempDir()
	providers, err := loadProviders(dir)
	if err != nil {
		t.Fatalf("loadProviders error: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("expected 0 providers, got %d", len(providers))
	}
}

func TestLoadProvidersMissingDir(t *testing.T) {
	_, err := loadProviders("/nonexistent/path")
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
}

func TestLoadProvidersInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("{{invalid yaml"), 0o644)

	_, err := loadProviders(dir)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestListenAddr(t *testing.T) {
	// Default port.
	t.Setenv("PORT", "")
	resetEnvCacheForTesting()
	if got := listenAddr(); got != ":3000" {
		t.Errorf("listenAddr() = %q, want :3000", got)
	}

	// Custom port.
	t.Setenv("PORT", "8080")
	resetEnvCacheForTesting()
	if got := listenAddr(); got != ":8080" {
		t.Errorf("listenAddr() = %q, want :8080", got)
	}
}

// --- transport / proxy tests ---

func TestProviderTransportProxySupport(t *testing.T) {
	// createProviderTransport must have a proxy function configured so that
	// HTTP_PROXY / HTTPS_PROXY / NO_PROXY env vars are honoured.
	transport := createProviderTransport(aggregator.ProviderConfig{})
	if transport.Proxy == nil {
		t.Error("createProviderTransport() should return a transport with Proxy function set")
	}
}

func TestProviderClientRedirectionsMax(t *testing.T) {
	// Server: / redirects to /final, /final returns 200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := aggregator.ProviderConfig{
		BaseURL:         srv.URL,
		RedirectionsMax:  1,
		Timeout:          5 * time.Second,
		Endpoints:        map[string]aggregator.EndpointConfig{"ep": {Path: "/"}},
	}
	client := createProviderClient(cfg)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()

	// With RedirectionsMax=1 we follow one redirect; the response is the redirect (302), not the final 200.
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 (redirect) when redirections_max=1", resp.StatusCode)
	}
}

// --- telemetry tests ---

func TestInitTracingDisabledSDK(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	resetEnvCacheForTesting()
	logger := zerolog.Nop()
	shutdown, err := initTracing(context.Background(), logger)
	if err != nil {
		t.Fatalf("initTracing error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown error: %v", err)
	}
}

func TestInitTracingExporterNone(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	resetEnvCacheForTesting()
	logger := zerolog.Nop()
	shutdown, err := initTracing(context.Background(), logger)
	if err != nil {
		t.Fatalf("initTracing error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown error: %v", err)
	}
}

func TestOtelServiceNameDefault(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	resetEnvCacheForTesting()
	if got := otelServiceName(); got != "ssfbff" {
		t.Errorf("otelServiceName() = %q, want %q", got, "ssfbff")
	}
}

func TestOtelServiceNameFromEnv(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "my-bff")
	resetEnvCacheForTesting()
	if got := otelServiceName(); got != "my-bff" {
		t.Errorf("otelServiceName() = %q, want %q", got, "my-bff")
	}
}

// --- correlation propagator tests ---

// isNoop reports whether a propagator injects nothing into a carrier.
// A noop propagator produces an empty carrier after Inject.
func isNoop(p propagation.TextMapPropagator) bool {
	carrier := propagation.MapCarrier{}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.SpanContext{})
	p.Inject(ctx, carrier)
	return len(carrier) == 0
}

func TestUpstreamPropagatorDefault(t *testing.T) {
	t.Setenv("OTEL_PROPAGATE_UPSTREAM", "")
	resetEnvCacheForTesting()
	// Default is the global propagator, which may or may not be noop depending
	// on whether initTracing has run. We only assert it is not nil.
	if upstreamPropagator() == nil {
		t.Error("upstreamPropagator() should never return nil")
	}
}

func TestUpstreamPropagatorDisabled(t *testing.T) {
	t.Setenv("OTEL_PROPAGATE_UPSTREAM", "false")
	resetEnvCacheForTesting()
	if !isNoop(upstreamPropagator()) {
		t.Error("upstreamPropagator() should be noop when OTEL_PROPAGATE_UPSTREAM=false")
	}
}

func TestDownstreamPropagatorDefault(t *testing.T) {
	t.Setenv("OTEL_PROPAGATE_DOWNSTREAM", "")
	resetEnvCacheForTesting()
	if downstreamPropagator() == nil {
		t.Error("downstreamPropagator() should never return nil")
	}
}

func TestDownstreamPropagatorDisabled(t *testing.T) {
	t.Setenv("OTEL_PROPAGATE_DOWNSTREAM", "false")
	resetEnvCacheForTesting()
	if !isNoop(downstreamPropagator()) {
		t.Error("downstreamPropagator() should be noop when OTEL_PROPAGATE_DOWNSTREAM=false")
	}
}
