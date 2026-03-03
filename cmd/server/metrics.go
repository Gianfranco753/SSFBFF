//go:build goexperiment.jsonv2

package main

import (
	"hash/fnv"
	"runtime"
	"strconv"
	"sync"
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

	// Async logging metrics
	asyncLogsDroppedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "async_logs_dropped_total",
			Help: "Total number of async log entries dropped (channel full or during shutdown)",
		},
	)

	// Health check metrics
	healthCheckDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "health_check_duration_seconds",
			Help:    "Duration of health check in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5},
		},
	)

	// Metrics batcher metrics
	metricsDroppedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "metrics_dropped_total",
			Help: "Total number of metrics dropped (batcher channel full or sampling)",
		},
		[]string{"reason"},
	)

	metricsBatcherChannelSize = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "metrics_batcher_channel_size",
			Help: "Current number of metrics in the batcher channel",
		},
	)

	// Shutdown metrics
	shutdownDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "shutdown_duration_seconds",
			Help:    "Duration of graceful shutdown in seconds",
			Buckets: []float64{0.1, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0},
		},
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

	// Label caches for common label combinations
	httpErrorCounterCache     sync.Map
	upstreamCallHistCache     sync.Map
	upstreamErrorCounterCache sync.Map
	aggregatorOpCounterCache  sync.Map
	labelCacheEnabled         bool
)

// Pre-format common status codes to avoid fmt.Sprintf
var statusCodeStrings = func() map[int]string {
	m := make(map[int]string, 20)
	for i := 400; i <= 599; i++ {
		m[i] = strconv.Itoa(i)
	}
	return m
}()

func getStatusCodeString(code int) string {
	if s, ok := statusCodeStrings[code]; ok {
		return s
	}
	return strconv.Itoa(code)
}

func hashLabels(labels []string) uint64 {
	h := fnv.New64a()
	for _, label := range labels {
		h.Write([]byte(label))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// metricsEnabled returns true if metrics recording is enabled.
var metricsEnabled = getCachedEnableMetrics()

func init() {
	labelCacheEnabled = getCachedMetricsLabelCacheEnabled()
	initMetricsBatcher()
}

func getHTTPErrorCounter(endpoint, method string, statusCode int) prometheus.Counter {
	if !labelCacheEnabled {
		return httpErrorsTotal.WithLabelValues(endpoint, method, getStatusCodeString(statusCode))
	}

	key := hashLabels([]string{endpoint, method, getStatusCodeString(statusCode)})
	if cached, ok := httpErrorCounterCache.Load(key); ok {
		return cached.(prometheus.Counter)
	}

	counter := httpErrorsTotal.WithLabelValues(endpoint, method, getStatusCodeString(statusCode))
	httpErrorCounterCache.Store(key, counter)
	return counter
}

// recordHTTPError records an HTTP error with endpoint, method, and status code.
func recordHTTPError(endpoint, method string, statusCode int) {
	if !metricsEnabled {
		return
	}
	if !shouldSample() {
		return
	}

	counter := getHTTPErrorCounter(endpoint, method, statusCode)
	if globalBatcher != nil && globalBatcher.enabled {
		globalBatcher.recordCounterInc(counter)
	} else {
		counter.Inc()
	}
}

func getUpstreamCallHistogram(provider, endpoint, status string) prometheus.Observer {
	if !labelCacheEnabled {
		return upstreamCallDuration.WithLabelValues(provider, endpoint, status)
	}

	key := hashLabels([]string{provider, endpoint, status})
	if cached, ok := upstreamCallHistCache.Load(key); ok {
		return cached.(prometheus.Observer)
	}

	hist := upstreamCallDuration.WithLabelValues(provider, endpoint, status)
	upstreamCallHistCache.Store(key, hist)
	return hist
}

// recordUpstreamCall records upstream call metrics.
func recordUpstreamCall(provider, endpoint string, duration time.Duration, status string) {
	if !metricsEnabled {
		return
	}
	if !shouldSample() {
		return
	}

	hist := getUpstreamCallHistogram(provider, endpoint, status)
	value := duration.Seconds()
	if globalBatcher != nil && globalBatcher.enabled {
		globalBatcher.recordHistogramObserve(hist, value)
	} else {
		hist.Observe(value)
	}
}

func getUpstreamErrorCounter(provider, endpoint, errorType string) prometheus.Counter {
	if !labelCacheEnabled {
		return upstreamErrorsTotal.WithLabelValues(provider, endpoint, errorType)
	}

	key := hashLabels([]string{provider, endpoint, errorType})
	if cached, ok := upstreamErrorCounterCache.Load(key); ok {
		return cached.(prometheus.Counter)
	}

	counter := upstreamErrorsTotal.WithLabelValues(provider, endpoint, errorType)
	upstreamErrorCounterCache.Store(key, counter)
	return counter
}

// recordUpstreamError records an upstream error.
func recordUpstreamError(provider, endpoint, errorType string) {
	if !metricsEnabled {
		return
	}
	if !shouldSample() {
		return
	}

	counter := getUpstreamErrorCounter(provider, endpoint, errorType)
	if globalBatcher != nil && globalBatcher.enabled {
		globalBatcher.recordCounterInc(counter)
	} else {
		counter.Inc()
	}
}

func getAggregatorOpCounter(status string) prometheus.Counter {
	if !labelCacheEnabled {
		return aggregatorOperationsTotal.WithLabelValues(status)
	}

	key := hashLabels([]string{status})
	if cached, ok := aggregatorOpCounterCache.Load(key); ok {
		return cached.(prometheus.Counter)
	}

	counter := aggregatorOperationsTotal.WithLabelValues(status)
	aggregatorOpCounterCache.Store(key, counter)
	return counter
}

// recordAggregatorOperation records aggregator operation status.
func recordAggregatorOperation(status string) {
	if !metricsEnabled {
		return
	}
	if !shouldSample() {
		return
	}

	counter := getAggregatorOpCounter(status)
	if globalBatcher != nil && globalBatcher.enabled {
		globalBatcher.recordCounterInc(counter)
	} else {
		counter.Inc()
	}
}

// resourceMetricsEnabled returns true if resource metrics collection is enabled.
var resourceMetricsEnabled = getCachedEnableResourceMetrics()

// recordAsyncLogsDropped records dropped async log entries.
func recordAsyncLogsDropped(count int) {
	if !metricsEnabled {
		return
	}
	if count <= 0 {
		return
	}
	asyncLogsDroppedTotal.Add(float64(count))
}

// recordHealthCheckDuration records the duration of a health check.
func recordHealthCheckDuration(duration time.Duration) {
	if !metricsEnabled {
		return
	}
	if !shouldSample() {
		return
	}
	healthCheckDuration.Observe(duration.Seconds())
}

// recordMetricsDropped records when metrics are dropped.
func recordMetricsDropped(reason string) {
	if !metricsEnabled {
		return
	}
	metricsDroppedTotal.WithLabelValues(reason).Inc()
}

// updateMetricsBatcherChannelSize updates the gauge for batcher channel size.
func updateMetricsBatcherChannelSize(size int) {
	if !metricsEnabled {
		return
	}
	metricsBatcherChannelSize.Set(float64(size))
}

// recordShutdownDuration records the duration of graceful shutdown.
func recordShutdownDuration(duration time.Duration) {
	if !metricsEnabled {
		return
	}
	if !shouldSample() {
		return
	}
	shutdownDuration.Observe(duration.Seconds())
}

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
