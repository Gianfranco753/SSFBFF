//go:build goexperiment.jsonv2

package main

import (
	"context"
	"time"

	"github.com/gcossani/ssfbff/internal/aggregator"
	"github.com/gcossani/ssfbff/runtime"
	"github.com/rs/zerolog"
)

// checkUpstreamHealth performs a quick health check on upstream services.
// It attempts to connect to each provider's base URL to verify connectivity.
// Only checks required (non-optional) providers - optional providers can fail without affecting health.
func checkUpstreamHealth(agg *aggregator.Aggregator) bool {
	if agg == nil {
		return false
	}
	
	// Get a sample endpoint from each required provider to test connectivity
	// This is a lightweight check - we just verify we can resolve and connect
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	
	// Create a test dependency for each required (non-optional) provider
	testDeps := make([]runtime.ProviderDep, 0)
	providers := agg.GetProviders()
	
	for name, prov := range providers {
		// Skip optional providers for health check
		if prov.Optional {
			continue
		}
		
		// Try to get the first endpoint for each required provider
		for endpointName := range prov.Endpoints {
			testDeps = append(testDeps, runtime.ProviderDep{
				Provider: name,
				Endpoint: endpointName,
			})
			break // Just test one endpoint per provider
		}
	}
	
	if len(testDeps) == 0 {
		// No required providers configured, consider healthy
		return true
	}
	
	// Try a quick fetch with a very short timeout
	// Only required providers are checked, so any failure means unhealthy
	_, err := agg.Fetch(ctx, testDeps)
	return err == nil
}

// setRouteLoggerIfAvailable sets the route logger if the generated SetRouteLogger function exists.
// This is a no-op if routes haven't been generated yet.
// The generated routes_gen.go will have a SetRouteLogger function that we can call.
func setRouteLoggerIfAvailable(logger zerolog.Logger) {
	// SetRouteLogger will be available in generated routes_gen.go after running go generate
	// We can't call it here directly because it's generated, but the generated code
	// will use the routeLogger variable that we set via this mechanism.
	// For now, this is a placeholder - the actual setting happens in generated code.
}
