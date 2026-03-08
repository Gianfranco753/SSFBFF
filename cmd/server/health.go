//go:build goexperiment.jsonv2

package main

import (
	"context"

	"github.com/gcossani/ssfbff/internal/aggregator"
	"github.com/gcossani/ssfbff/runtime"
)

// ProviderHealth represents the health status of a single provider.
type ProviderHealth struct {
	Healthy   bool   `json:"healthy"`
	Status    string `json:"status"`    // "healthy", "unhealthy", "unchecked"
	Error     string `json:"error,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
}

// HealthStatus represents the overall health status of the BFF and its upstream providers.
type HealthStatus struct {
	Healthy          bool                       `json:"healthy"`
	FailureThreshold int                        `json:"failure_threshold"`
	FailureCount     int                        `json:"failure_count"`
	TotalRequired    int                        `json:"total_required"`
	Providers        map[string]ProviderHealth `json:"providers"`
}

// checkUpstreamHealth performs a health check on upstream services and returns detailed status.
// It checks each required (non-optional) provider individually and tracks per-provider status.
// The overall health is determined by comparing failure count against the failure threshold.
func checkUpstreamHealth(agg *aggregator.Aggregator) HealthStatus {
	status := HealthStatus{
		Healthy:          true,
		FailureThreshold: getCachedHealthCheckFailureThreshold(),
		Providers:        make(map[string]ProviderHealth),
	}

	if agg == nil {
		status.Healthy = false
		return status
	}

	providers := agg.GetProviders()
	timeout := getCachedHealthCheckTimeout()

	// Check each required provider individually
	for name, prov := range providers {
		// Skip optional providers for health check
		if prov.Optional {
			status.Providers[name] = ProviderHealth{
				Healthy: true,
				Status:  "unchecked",
			}
			continue
		}

		// Skip providers with no endpoints (log warning, don't fail health check)
		if len(prov.Endpoints) == 0 {
			status.Providers[name] = ProviderHealth{
				Healthy: true,
				Status:  "unchecked",
			}
			continue
		}

		// Get the first endpoint for this provider
		var endpointName string
		for epName := range prov.Endpoints {
			endpointName = epName
			break
		}

		status.TotalRequired++

		// Perform health check with short timeout
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		testDep := runtime.ProviderDep{
			Provider: name,
			Endpoint: endpointName,
		}
		_, err := agg.Fetch(ctx, []runtime.ProviderDep{testDep}, nil)
		cancel()

		if err != nil {
			status.Providers[name] = ProviderHealth{
				Healthy:  false,
				Status:   "unhealthy",
				Error:    err.Error(),
				Endpoint: endpointName,
			}
			status.FailureCount++
		} else {
			status.Providers[name] = ProviderHealth{
				Healthy:  true,
				Status:   "healthy",
				Endpoint: endpointName,
			}
		}
	}

	// Determine overall health based on failure threshold
	if status.TotalRequired == 0 {
		// No required providers configured, consider healthy
		status.Healthy = true
	} else {
		// Healthy if failure count is within threshold
		status.Healthy = status.FailureCount <= status.FailureThreshold
	}

	return status
}
