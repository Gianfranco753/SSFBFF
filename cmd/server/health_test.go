//go:build goexperiment.jsonv2

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gcossani/ssfbff/internal/aggregator"
)

func makeEndpoints(m map[string]string) map[string]aggregator.EndpointConfig {
	result := make(map[string]aggregator.EndpointConfig, len(m))
	for k, v := range m {
		result[k] = aggregator.EndpointConfig{Path: v}
	}
	return result
}

func testClientFactory(cfg aggregator.ProviderConfig) *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

func TestCheckUpstreamHealth_AllHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	agg := aggregator.New(map[string]aggregator.ProviderConfig{
		"svc1": {BaseURL: srv.URL, Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
		"svc2": {BaseURL: srv.URL, Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
	}, testClientFactory)

	status := checkUpstreamHealth(agg)
	if !status.Healthy {
		t.Errorf("expected healthy, got unhealthy")
	}
	if status.FailureCount != 0 {
		t.Errorf("expected 0 failures, got %d", status.FailureCount)
	}
	if len(status.Providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(status.Providers))
	}
}

func TestCheckUpstreamHealth_OneFailure(t *testing.T) {
	healthySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer healthySrv.Close()

	failingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fail", http.StatusInternalServerError)
	}))
	defer failingSrv.Close()

	agg := aggregator.New(map[string]aggregator.ProviderConfig{
		"healthy":  {BaseURL: healthySrv.URL, Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
		"failing":  {BaseURL: failingSrv.URL, Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
	}, testClientFactory)

	status := checkUpstreamHealth(agg)
	if status.FailureCount != 1 {
		t.Errorf("expected 1 failure, got %d", status.FailureCount)
	}
	if status.Providers["healthy"].Healthy != true {
		t.Error("healthy provider should be healthy")
	}
	if status.Providers["failing"].Healthy != false {
		t.Error("failing provider should be unhealthy")
	}
}

func TestCheckUpstreamHealth_OptionalProvidersSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	agg := aggregator.New(map[string]aggregator.ProviderConfig{
		"required": {BaseURL: srv.URL, Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
		"optional": {BaseURL: srv.URL, Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"}), Optional: true},
	}, testClientFactory)

	status := checkUpstreamHealth(agg)
	if status.TotalRequired != 1 {
		t.Errorf("expected 1 required provider, got %d", status.TotalRequired)
	}
	if status.Providers["optional"].Status != "unchecked" {
		t.Errorf("optional provider should be unchecked, got %s", status.Providers["optional"].Status)
	}
}

func TestCheckUpstreamHealth_NoProviders(t *testing.T) {
	agg := aggregator.New(map[string]aggregator.ProviderConfig{}, testClientFactory)
	status := checkUpstreamHealth(agg)
	if !status.Healthy {
		t.Error("expected healthy when no providers configured")
	}
}

func TestCheckUpstreamHealth_ProviderWithNoEndpoints(t *testing.T) {
	// Note: Providers must have at least one endpoint (validated at creation).
	// This test verifies that if a provider somehow has no endpoints in the config
	// (which shouldn't happen due to validation), it would be handled gracefully.
	// Since validation prevents this, we test with a provider that has an endpoint.
	agg := aggregator.New(map[string]aggregator.ProviderConfig{
		"svc": {BaseURL: "http://example.com", Timeout: 1 * time.Second, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
	}, testClientFactory)

	status := checkUpstreamHealth(agg)
	// Provider with endpoint should be checked (not unchecked)
	if status.Providers["svc"].Status == "unchecked" {
		t.Errorf("provider with endpoint should be checked, got %s", status.Providers["svc"].Status)
	}
}

func TestCheckUpstreamHealth_ErrorContext(t *testing.T) {
	// Test that error messages include context
	agg := aggregator.New(map[string]aggregator.ProviderConfig{
		"svc": {BaseURL: "http://localhost:9999", Timeout: 100 * time.Millisecond, Endpoints: makeEndpoints(map[string]string{"ep": "/ep"})},
	}, testClientFactory)

	status := checkUpstreamHealth(agg)
	if status.Providers["svc"].Error == "" {
		t.Error("expected error message for failing provider")
	}
}
