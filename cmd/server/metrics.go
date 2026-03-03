//go:build goexperiment.jsonv2

package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)


var (
	// HTTP error counter by endpoint and status code
	httpErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_errors_total",
			Help: "Total number of HTTP errors by endpoint and status code",
		},
		[]string{"endpoint", "method", "status_code"},
	)

	// Upstream call duration histogram by provider
	upstreamCallDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "upstream_call_duration_seconds",
			Help:    "Duration of upstream HTTP calls in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0},
		},
		[]string{"provider", "endpoint", "status"},
	)

	// Upstream error counter by provider
	upstreamErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "upstream_errors_total",
			Help: "Total number of upstream errors by provider and endpoint",
		},
		[]string{"provider", "endpoint", "error_type"},
	)

	// Aggregator operation metrics
	aggregatorOperationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aggregator_operations_total",
			Help: "Total number of aggregator operations by status",
		},
		[]string{"status"},
	)

	// Resource metrics
	goRoutines = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "go_goroutines",
			Help: "Number of goroutines that currently exist",
		},
	)

	goMemoryAllocBytes = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "go_memstats_alloc_bytes",
			Help: "Number of bytes allocated and still in use",
		},
	)

	goMemorySysBytes = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "go_memstats_sys_bytes",
			Help: "Number of bytes obtained from system",
		},
	)
)

// getEnvBool reads a boolean environment variable, returning defaultValue if not set or invalid.
func getEnvBool(key string, defaultValue bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(val)
	if err != nil {
		return defaultValue
	}
	return parsed
}

// metricsEnabled returns true if metrics recording is enabled.
var metricsEnabled = getEnvBool("ENABLE_METRICS", true)

// recordHTTPError records an HTTP error with endpoint, method, and status code.
func recordHTTPError(endpoint, method string, statusCode int) {
	if !metricsEnabled {
		return
	}
	httpErrorsTotal.WithLabelValues(endpoint, method, fmt.Sprintf("%d", statusCode)).Inc()
}

// recordUpstreamCall records upstream call metrics.
func recordUpstreamCall(provider, endpoint string, duration time.Duration, status string) {
	if !metricsEnabled {
		return
	}
	upstreamCallDuration.WithLabelValues(provider, endpoint, status).Observe(duration.Seconds())
}

// recordUpstreamError records an upstream error.
func recordUpstreamError(provider, endpoint, errorType string) {
	if !metricsEnabled {
		return
	}
	upstreamErrorsTotal.WithLabelValues(provider, endpoint, errorType).Inc()
}

// recordAggregatorOperation records aggregator operation status.
func recordAggregatorOperation(status string) {
	if !metricsEnabled {
		return
	}
	aggregatorOperationsTotal.WithLabelValues(status).Inc()
}

// resourceMetricsEnabled returns true if resource metrics collection is enabled.
var resourceMetricsEnabled = getEnvBool("ENABLE_RESOURCE_METRICS", true)

// updateResourceMetrics updates resource metrics (goroutines, memory).
// Uses runtime.ReadMemStats which is expensive (stop-the-world), so this should be called infrequently.
func updateResourceMetrics() {
	if !resourceMetricsEnabled {
		return
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	goRoutines.Set(float64(runtime.NumGoroutine()))
	goMemoryAllocBytes.Set(float64(m.Alloc))
	goMemorySysBytes.Set(float64(m.Sys))
}
