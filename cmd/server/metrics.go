//go:build goexperiment.jsonv2

package main

import (
	"hash"
	"hash/fnv"
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

	// HTTP request duration histogram by endpoint, method, and status code
	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duration of HTTP requests in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
		},
		[]string{"endpoint", "method", "status_code"},
	)

	// HTTP response size histogram by endpoint, method, and status code
	httpResponseSizeBytes = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_response_size_bytes",
			Help:    "Size of HTTP response bodies in bytes",
			Buckets: []float64{100, 500, 1000, 5000, 10000, 50000, 100000, 500000, 1000000, 5000000},
		},
		[]string{"endpoint", "method", "status_code"},
	)

	// Slow requests counter by endpoint and method
	slowRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "slow_requests_total",
			Help: "Total number of slow requests that exceeded the threshold",
		},
		[]string{"endpoint", "method"},
	)

	// Label caches for common label combinations
	httpErrorCounterCache     sync.Map
	upstreamCallHistCache     sync.Map
	upstreamErrorCounterCache sync.Map
	aggregatorOpCounterCache  sync.Map
	httpRequestDurationCache  sync.Map
	httpResponseSizeCache     sync.Map
	slowRequestCounterCache   sync.Map
	labelCacheEnabled         bool

	// hasherPool reuses hash.Hash64 instances to avoid allocations in hashLabels()
	hasherPool = sync.Pool{
		New: func() interface{} { return fnv.New64a() },
	}
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
	h := hasherPool.Get().(hash.Hash64)
	defer hasherPool.Put(h)
	h.Reset()
	for _, label := range labels {
		h.Write([]byte(label))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// getCachedCounter returns a cached Counter or creates and caches a new one.
// cache is the sync.Map cache, metricVec is the CounterVec, labels are the label values.
func getCachedCounter(cache *sync.Map, metricVec *prometheus.CounterVec, labels []string) prometheus.Counter {
	if !labelCacheEnabled {
		return metricVec.WithLabelValues(labels...)
	}

	key := hashLabels(labels)
	if cached, ok := cache.Load(key); ok {
		return cached.(prometheus.Counter)
	}

	counter := metricVec.WithLabelValues(labels...)
	cache.Store(key, counter)
	return counter
}

// getCachedObserver returns a cached Observer or creates and caches a new one.
// cache is the sync.Map cache, metricVec is the HistogramVec, labels are the label values.
func getCachedObserver(cache *sync.Map, metricVec *prometheus.HistogramVec, labels []string) prometheus.Observer {
	if !labelCacheEnabled {
		return metricVec.WithLabelValues(labels...)
	}

	key := hashLabels(labels)
	if cached, ok := cache.Load(key); ok {
		return cached.(prometheus.Observer)
	}

	observer := metricVec.WithLabelValues(labels...)
	cache.Store(key, observer)
	return observer
}

// metricsEnabled returns true if metrics recording is enabled.
var metricsEnabled = getCachedEnableMetrics()

func init() {
	labelCacheEnabled = getCachedMetricsLabelCacheEnabled()
	initMetricsBatcher()
}

func getHTTPErrorCounter(endpoint, method string, statusCode int) prometheus.Counter {
	return getCachedCounter(&httpErrorCounterCache, httpErrorsTotal, []string{endpoint, method, getStatusCodeString(statusCode)})
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
	return getCachedObserver(&upstreamCallHistCache, upstreamCallDuration, []string{provider, endpoint, status})
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
	return getCachedCounter(&upstreamErrorCounterCache, upstreamErrorsTotal, []string{provider, endpoint, errorType})
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
	return getCachedCounter(&aggregatorOpCounterCache, aggregatorOperationsTotal, []string{status})
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

func getHTTPRequestDurationHistogram(endpoint, method string, statusCode int) prometheus.Observer {
	return getCachedObserver(&httpRequestDurationCache, httpRequestDuration, []string{endpoint, method, getStatusCodeString(statusCode)})
}

// recordHTTPRequestDuration records the duration of an HTTP request.
func recordHTTPRequestDuration(endpoint, method string, statusCode int, duration time.Duration) {
	if !metricsEnabled {
		return
	}
	if !shouldSample() {
		return
	}

	hist := getHTTPRequestDurationHistogram(endpoint, method, statusCode)
	value := duration.Seconds()
	if globalBatcher != nil && globalBatcher.enabled {
		globalBatcher.recordHistogramObserve(hist, value)
	} else {
		hist.Observe(value)
	}
}

func getHTTPResponseSizeHistogram(endpoint, method string, statusCode int) prometheus.Observer {
	return getCachedObserver(&httpResponseSizeCache, httpResponseSizeBytes, []string{endpoint, method, getStatusCodeString(statusCode)})
}

// recordHTTPResponseSize records the size of an HTTP response body.
func recordHTTPResponseSize(endpoint, method string, statusCode int, sizeBytes int) {
	if !metricsEnabled {
		return
	}
	if !shouldSample() {
		return
	}

	hist := getHTTPResponseSizeHistogram(endpoint, method, statusCode)
	value := float64(sizeBytes)
	if globalBatcher != nil && globalBatcher.enabled {
		globalBatcher.recordHistogramObserve(hist, value)
	} else {
		hist.Observe(value)
	}
}

func getSlowRequestCounter(endpoint, method string) prometheus.Counter {
	return getCachedCounter(&slowRequestCounterCache, slowRequestsTotal, []string{endpoint, method})
}

// recordSlowRequest records a slow request that exceeded the threshold.
func recordSlowRequest(endpoint, method string) {
	if !metricsEnabled {
		return
	}
	if !shouldSample() {
		return
	}

	counter := getSlowRequestCounter(endpoint, method)
	if globalBatcher != nil && globalBatcher.enabled {
		globalBatcher.recordCounterInc(counter)
	} else {
		counter.Inc()
	}
}
