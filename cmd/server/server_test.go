//go:build goexperiment.jsonv2

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
