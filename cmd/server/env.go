//go:build goexperiment.jsonv2

package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// envCache holds all environment variables cached at startup.
// This eliminates repeated os.Getenv() syscalls, especially in hot paths.
var (
	envCache struct {
		mu sync.RWMutex

		// Server configuration
		port                string
		dataDir             string
		maxIdleConnsPerHost int
		maxConnsPerHost     int
		idleConnTimeout     time.Duration
		dialTimeout         time.Duration
		keepAlive           time.Duration

		// Fiber configuration
		fiberPrefork      bool
		fiberConcurrency  int
		fiberBodyLimit    int
		fiberReadTimeout  time.Duration
		fiberWriteTimeout time.Duration
		fiberIdleTimeout  time.Duration

		// Logging configuration
		logLevel               string
		logFormat              string
		asyncLogging           bool
		enableErrorLogging     bool
		asyncLoggingBufferSize int

		// OpenTelemetry configuration
		otelSDKDisabled                bool
		otelTracesExporter             string
		otelDisableTracing             bool
		otelServiceName                string
		otelPropagateUpstream          bool
		otelPropagateDownstream        bool
		otelExporterOTLPEndpoint       string
		otelExporterOTLPTracesEndpoint string

		// Metrics configuration
		enableMetrics             bool
		metricsLabelCacheEnabled  bool
		metricsCacheTTL           int
		metricsBatchingEnabled    bool
		metricsBatchSize          int
		metricsBatchInterval      time.Duration
		metricsSampleRate         float64
		metricsBatcherChannelSize int

		// Middleware configuration
		useTraceIDAsRequestID bool

		// Health check configuration
		healthCheckTimeout          time.Duration
		healthCheckFailureThreshold int
		readySkipUpstreamCheck      bool

		// Slow request configuration
		slowRequestThreshold time.Duration

		// Shutdown configuration
		shutdownTimeout time.Duration

		// Documentation configuration
		enableDocs bool

		// Response body size limit
		maxResponseBodySize int

		// Proxy configuration (cached to avoid http.ProxyFromEnvironment overhead)
		httpProxy  *url.URL
		httpsProxy *url.URL
		noProxy    []string
		proxyFunc  func(*http.Request) (*url.URL, error)

		// OpenFeature configuration
		openFeatureEnabled bool
		openFeatureCacheTTL time.Duration
		openFeatureCache struct {
			mu      sync.RWMutex
			entries map[string]cacheEntry
		}
	}
	envCacheOnce sync.Once
)

// cacheEntry holds a cached flag value with expiration time
type cacheEntry struct {
	value   interface{}
	expires time.Time
}

// initEnvCache loads all environment variables into the cache at startup.
// This is called via init() to ensure it runs before any package-level variable initializations.
func initEnvCache() {
	envCache.mu.Lock()
	defer envCache.mu.Unlock()

	// Initialize OpenFeature if configured
	envCache.openFeatureEnabled = initOpenFeature()
	if envCache.openFeatureEnabled {
		envCache.openFeatureCacheTTL = openFeatureCfg.cacheTTL
		envCache.openFeatureCache.entries = make(map[string]cacheEntry)
	}

	// Server configuration
	envCache.port = getEnvString("PORT", "3000")
	envCache.dataDir = getEnvString("DATA_DIR", "data")
	envCache.maxIdleConnsPerHost = getEnvInt("MAX_IDLE_CONNS_PER_HOST", 2000)
	envCache.maxConnsPerHost = getEnvInt("MAX_CONNS_PER_HOST", 5000)
	envCache.idleConnTimeout = getEnvDuration("IDLE_CONN_TIMEOUT", 90*time.Second)
	envCache.dialTimeout = getEnvDuration("DIAL_TIMEOUT", 3*time.Second)
	envCache.keepAlive = getEnvDuration("KEEP_ALIVE", 30*time.Second)

	// Fiber configuration
	// Prefork disabled by default for containerized deployments.
	// In Docker/Kubernetes, scale horizontally (multiple containers) rather than vertically (multiple processes).
	// Enable prefork only if you have multiple dedicated CPU cores per container and aren't using an orchestrator.
	envCache.fiberPrefork = getEnvBool("FIBER_PREFORK", false)
	envCache.fiberConcurrency = getEnvInt("FIBER_CONCURRENCY", 0) // 0 means use default calculation
	envCache.fiberBodyLimit = getEnvInt("FIBER_BODY_LIMIT", 10*1024*1024)
	envCache.fiberReadTimeout = getEnvDuration("FIBER_READ_TIMEOUT", 5*time.Second)
	envCache.fiberWriteTimeout = getEnvDuration("FIBER_WRITE_TIMEOUT", 10*time.Second)
	envCache.fiberIdleTimeout = getEnvDuration("FIBER_IDLE_TIMEOUT", 120*time.Second)

	// Logging configuration
	envCache.logLevel = getEnvString("LOG_LEVEL", "info")
	envCache.logFormat = getEnvString("LOG_FORMAT", "json")
	envCache.asyncLogging = getEnvBool("ASYNC_LOGGING", true) // Always async by default
	envCache.enableErrorLogging = getEnvBool("ENABLE_ERROR_LOGGING", true)
	envCache.asyncLoggingBufferSize = getEnvInt("ASYNC_LOGGING_BUFFER_SIZE", 1000)

	// OpenTelemetry configuration
	envCache.otelSDKDisabled = getEnvBool("OTEL_SDK_DISABLED", false)
	envCache.otelTracesExporter = getEnvString("OTEL_TRACES_EXPORTER", "")
	envCache.otelDisableTracing = getEnvBool("OTEL_DISABLE_TRACING", false)
	envCache.otelServiceName = getEnvString("OTEL_SERVICE_NAME", "ssfbff")
	envCache.otelPropagateUpstream = getEnvBool("OTEL_PROPAGATE_UPSTREAM", true)
	envCache.otelPropagateDownstream = getEnvBool("OTEL_PROPAGATE_DOWNSTREAM", true)
	envCache.otelExporterOTLPEndpoint = getEnvString("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	envCache.otelExporterOTLPTracesEndpoint = getEnvString("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	// Metrics configuration
	envCache.enableMetrics = getEnvBool("ENABLE_METRICS", true)
	envCache.metricsLabelCacheEnabled = getEnvBool("METRICS_LABEL_CACHE_ENABLED", true)
	envCache.metricsCacheTTL = getEnvInt("METRICS_CACHE_TTL", 0)
	envCache.metricsBatchingEnabled = getEnvBool("METRICS_BATCHING_ENABLED", true)
	envCache.metricsBatchSize = getEnvInt("METRICS_BATCH_SIZE", 1000)
	envCache.metricsBatchInterval = getEnvDuration("METRICS_BATCH_INTERVAL", 100*time.Millisecond)
	envCache.metricsSampleRate = getEnvFloat("METRICS_SAMPLE_RATE", 1.0)
	// Default channel size is 10x batch size to handle high throughput (650k req/s)
	// This prevents channel saturation and fallback to synchronous recording
	envCache.metricsBatcherChannelSize = getEnvInt("METRICS_BATCHER_CHANNEL_SIZE", 0) // 0 means use default (batchSize * 10)

	// Middleware configuration
	envCache.useTraceIDAsRequestID = getEnvBool("USE_TRACE_ID_AS_REQUEST_ID", true)

	// Health check configuration
	envCache.healthCheckTimeout = getEnvDuration("HEALTH_CHECK_TIMEOUT", 500*time.Millisecond)
	envCache.healthCheckFailureThreshold = getEnvInt("HEALTH_CHECK_FAILURE_THRESHOLD", 0)
	envCache.readySkipUpstreamCheck = getEnvBool("READY_SKIP_UPSTREAM_CHECK", false)

	// Slow request configuration
	envCache.slowRequestThreshold = getEnvDuration("SLOW_REQUEST_THRESHOLD", 1*time.Second)

	// Shutdown configuration
	envCache.shutdownTimeout = getEnvDuration("SHUTDOWN_TIMEOUT", 30*time.Second)

	// Documentation configuration
	envCache.enableDocs = getEnvBool("ENABLE_DOCS", true)

	// Response body size limit
	envCache.maxResponseBodySize = getEnvInt("MAX_RESPONSE_BODY_SIZE", 10*1024*1024)

	// Proxy configuration - parse once and cache
	envCache.httpProxy = parseProxyURL(getEnvString("HTTP_PROXY", ""))
	httpsProxyStr := getEnvString("HTTPS_PROXY", "")
	if httpsProxyStr == "" {
		// HTTPS_PROXY falls back to HTTP_PROXY if not set
		envCache.httpsProxy = envCache.httpProxy
	} else {
		envCache.httpsProxy = parseProxyURL(httpsProxyStr)
	}
	noProxyStr := getEnvString("NO_PROXY", "")
	if noProxyStr != "" {
		envCache.noProxy = strings.Split(noProxyStr, ",")
		for i := range envCache.noProxy {
			envCache.noProxy[i] = strings.TrimSpace(envCache.noProxy[i])
		}
	}

	// Create cached proxy function
	envCache.proxyFunc = createCachedProxyFunc(envCache.httpProxy, envCache.httpsProxy, envCache.noProxy)
}

// ensureCacheInitialized ensures the cache is initialized before accessing it.
// This is safe to call multiple times and is thread-safe.
func ensureCacheInitialized() {
	envCacheOnce.Do(initEnvCache)
}

// Helper functions for OpenFeature flag evaluation with push/streaming priority

// getCachedFlag is a generic helper that gets a cached flag value from OpenFeature
// or falls back to the environment variable value.
// T is the type of the flag value (string, int, bool, float64).
// evaluateFn is a function that evaluates the flag and returns (value, found).
func getCachedFlag[T comparable](flagKey string, envVarValue T, evaluateFn func(context.Context, string) (T, bool)) T {
	if !envCache.openFeatureEnabled {
		return envVarValue
	}

	ctx := context.Background()

	// Check cache first (entries are invalidated by push/streaming events, TTL is fallback)
	if envCache.openFeatureCacheTTL > 0 {
		envCache.openFeatureCache.mu.RLock()
		if entry, ok := envCache.openFeatureCache.entries[flagKey]; ok {
			if time.Now().Before(entry.expires) {
				value := entry.value.(T)
				envCache.openFeatureCache.mu.RUnlock()
				return value
			}
		}
		envCache.openFeatureCache.mu.RUnlock()
	}

	// Evaluate flag (cache miss or expired)
	if flagValue, found := evaluateFn(ctx, flagKey); found {
		// Cache the value (TTL used as fallback when events not available)
		if envCache.openFeatureCacheTTL > 0 {
			envCache.openFeatureCache.mu.Lock()
			envCache.openFeatureCache.entries[flagKey] = cacheEntry{
				value:   flagValue,
				expires: time.Now().Add(envCache.openFeatureCacheTTL),
			}
			envCache.openFeatureCache.mu.Unlock()
		}
		return flagValue
	}

	return envVarValue
}

// getCachedFlagString gets a string value from OpenFeature (push/streaming updates via events, TTL as fallback) or falls back to env var
func getCachedFlagString(flagKey string, envVarValue string) string {
	return getCachedFlag(flagKey, envVarValue, evaluateFlagString)
}

// getCachedFlagInt gets an int value from OpenFeature (push/streaming updates via events, TTL as fallback) or falls back to env var
func getCachedFlagInt(flagKey string, envVarValue int) int {
	return getCachedFlag(flagKey, envVarValue, evaluateFlagInt)
}

// getCachedFlagBool gets a bool value from OpenFeature (push/streaming updates via events, TTL as fallback) or falls back to env var
func getCachedFlagBool(flagKey string, envVarValue bool) bool {
	return getCachedFlag(flagKey, envVarValue, evaluateFlagBool)
}

// evaluateFlagDuration evaluates a duration flag by parsing a string value
func evaluateFlagDuration(ctx context.Context, flagKey string) (time.Duration, bool) {
	if flagValue, found := evaluateFlagString(ctx, flagKey); found {
		if parsed, err := time.ParseDuration(flagValue); err == nil && parsed > 0 {
			return parsed, true
		}
	}
	return 0, false
}

// getCachedFlagDuration gets a duration value from OpenFeature (push/streaming updates via events, TTL as fallback) or falls back to env var
func getCachedFlagDuration(flagKey string, envVarValue time.Duration) time.Duration {
	return getCachedFlag(flagKey, envVarValue, evaluateFlagDuration)
}

// getCachedFlagFloat gets a float value from OpenFeature (push/streaming updates via events, TTL as fallback) or falls back to env var
func getCachedFlagFloat(flagKey string, envVarValue float64) float64 {
	return getCachedFlag(flagKey, envVarValue, evaluateFlagFloat)
}

// Helper functions to get cached values.
// These functions now check OpenFeature first (with TTL caching), then fall back to env vars.

func getCachedPort() string {
	ensureCacheInitialized()
	return getCachedFlagString("PORT", envCache.port)
}

func getCachedDataDir() string {
	ensureCacheInitialized()
	return getCachedFlagString("DATA_DIR", envCache.dataDir)
}

func getCachedMaxIdleConnsPerHost() int {
	ensureCacheInitialized()
	return getCachedFlagInt("MAX_IDLE_CONNS_PER_HOST", envCache.maxIdleConnsPerHost)
}

func getCachedMaxConnsPerHost() int {
	ensureCacheInitialized()
	return getCachedFlagInt("MAX_CONNS_PER_HOST", envCache.maxConnsPerHost)
}

func getCachedIdleConnTimeout() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("IDLE_CONN_TIMEOUT", envCache.idleConnTimeout)
}

func getCachedDialTimeout() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("DIAL_TIMEOUT", envCache.dialTimeout)
}

func getCachedKeepAlive() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("KEEP_ALIVE", envCache.keepAlive)
}

func getCachedFiberPrefork() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("FIBER_PREFORK", envCache.fiberPrefork)
}

func getCachedFiberConcurrency() int {
	ensureCacheInitialized()
	return getCachedFlagInt("FIBER_CONCURRENCY", envCache.fiberConcurrency)
}

func getCachedFiberBodyLimit() int {
	ensureCacheInitialized()
	return getCachedFlagInt("FIBER_BODY_LIMIT", envCache.fiberBodyLimit)
}

func getCachedFiberReadTimeout() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("FIBER_READ_TIMEOUT", envCache.fiberReadTimeout)
}

func getCachedFiberWriteTimeout() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("FIBER_WRITE_TIMEOUT", envCache.fiberWriteTimeout)
}

func getCachedFiberIdleTimeout() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("FIBER_IDLE_TIMEOUT", envCache.fiberIdleTimeout)
}

func getCachedLogLevel() string {
	ensureCacheInitialized()
	return getCachedFlagString("LOG_LEVEL", envCache.logLevel)
}

func getCachedLogFormat() string {
	ensureCacheInitialized()
	return getCachedFlagString("LOG_FORMAT", envCache.logFormat)
}

func getCachedEnableErrorLogging() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("ENABLE_ERROR_LOGGING", envCache.enableErrorLogging)
}

func getCachedAsyncLoggingBufferSize() int {
	ensureCacheInitialized()
	return getCachedFlagInt("ASYNC_LOGGING_BUFFER_SIZE", envCache.asyncLoggingBufferSize)
}

func getCachedOtelSDKDisabled() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("OTEL_SDK_DISABLED", envCache.otelSDKDisabled)
}

func getCachedOtelTracesExporter() string {
	ensureCacheInitialized()
	return getCachedFlagString("OTEL_TRACES_EXPORTER", envCache.otelTracesExporter)
}

func getCachedOtelDisableTracing() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("OTEL_DISABLE_TRACING", envCache.otelDisableTracing)
}

func getCachedOtelServiceName() string {
	ensureCacheInitialized()
	return getCachedFlagString("OTEL_SERVICE_NAME", envCache.otelServiceName)
}

func getCachedOtelPropagateUpstream() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("OTEL_PROPAGATE_UPSTREAM", envCache.otelPropagateUpstream)
}

func getCachedOtelPropagateDownstream() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("OTEL_PROPAGATE_DOWNSTREAM", envCache.otelPropagateDownstream)
}

func getCachedOtelExporterOTLPEndpoint() string {
	ensureCacheInitialized()
	return getCachedFlagString("OTEL_EXPORTER_OTLP_ENDPOINT", envCache.otelExporterOTLPEndpoint)
}

func getCachedOtelExporterOTLPTracesEndpoint() string {
	ensureCacheInitialized()
	return getCachedFlagString("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", envCache.otelExporterOTLPTracesEndpoint)
}

func getCachedEnableMetrics() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("ENABLE_METRICS", envCache.enableMetrics)
}

func getCachedMetricsLabelCacheEnabled() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("METRICS_LABEL_CACHE_ENABLED", envCache.metricsLabelCacheEnabled)
}

func getCachedMetricsCacheTTL() int {
	ensureCacheInitialized()
	return getCachedFlagInt("METRICS_CACHE_TTL", envCache.metricsCacheTTL)
}

func getCachedMetricsBatchingEnabled() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("METRICS_BATCHING_ENABLED", envCache.metricsBatchingEnabled)
}

func getCachedMetricsBatchSize() int {
	ensureCacheInitialized()
	return getCachedFlagInt("METRICS_BATCH_SIZE", envCache.metricsBatchSize)
}

func getCachedMetricsBatchInterval() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("METRICS_BATCH_INTERVAL", envCache.metricsBatchInterval)
}

func getCachedMetricsSampleRate() float64 {
	ensureCacheInitialized()
	return getCachedFlagFloat("METRICS_SAMPLE_RATE", envCache.metricsSampleRate)
}

func getCachedMetricsBatcherChannelSize() int {
	ensureCacheInitialized()
	return getCachedFlagInt("METRICS_BATCHER_CHANNEL_SIZE", envCache.metricsBatcherChannelSize)
}

func getCachedUseTraceIDAsRequestID() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("USE_TRACE_ID_AS_REQUEST_ID", envCache.useTraceIDAsRequestID)
}

func getCachedHealthCheckTimeout() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("HEALTH_CHECK_TIMEOUT", envCache.healthCheckTimeout)
}

func getCachedHealthCheckFailureThreshold() int {
	ensureCacheInitialized()
	return getCachedFlagInt("HEALTH_CHECK_FAILURE_THRESHOLD", envCache.healthCheckFailureThreshold)
}

func getCachedReadySkipUpstreamCheck() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("READY_SKIP_UPSTREAM_CHECK", envCache.readySkipUpstreamCheck)
}

func getCachedSlowRequestThreshold() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("SLOW_REQUEST_THRESHOLD", envCache.slowRequestThreshold)
}

func getCachedShutdownTimeout() time.Duration {
	ensureCacheInitialized()
	return getCachedFlagDuration("SHUTDOWN_TIMEOUT", envCache.shutdownTimeout)
}

func getCachedEnableDocs() bool {
	ensureCacheInitialized()
	return getCachedFlagBool("ENABLE_DOCS", envCache.enableDocs)
}

func getCachedMaxResponseBodySize() int {
	ensureCacheInitialized()
	return getCachedFlagInt("MAX_RESPONSE_BODY_SIZE", envCache.maxResponseBodySize)
}

func getCachedProxyFunc() func(*http.Request) (*url.URL, error) {
	ensureCacheInitialized()
	return envCache.proxyFunc
}

// Helper functions for parsing env vars (used only during init)

func getEnvString(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(val)
	if err != nil {
		return defaultValue
	}
	return parsed
}

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

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}
	parsed, err := time.ParseDuration(val)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func getEnvFloat(key string, defaultValue float64) float64 {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return defaultValue
	}
	return parsed
}

// parseProxyURL parses a proxy URL string, returning nil if empty or invalid.
func parseProxyURL(proxyStr string) *url.URL {
	if proxyStr == "" {
		return nil
	}
	proxyURL, err := url.Parse(proxyStr)
	if err != nil {
		return nil
	}
	return proxyURL
}

// createCachedProxyFunc creates a proxy function that uses cached proxy settings.
// This replaces http.ProxyFromEnvironment to avoid per-request env var reads.
func createCachedProxyFunc(httpProxy, httpsProxy *url.URL, noProxy []string) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		// Check NO_PROXY first
		if len(noProxy) > 0 {
			host := req.URL.Hostname()
			for _, pattern := range noProxy {
				if matchesNoProxy(host, pattern) {
					return nil, nil
				}
			}
		}

		// Select proxy based on scheme
		if req.URL.Scheme == "https" {
			if httpsProxy != nil {
				return httpsProxy, nil
			}
		}
		if httpProxy != nil {
			return httpProxy, nil
		}

		return nil, nil
	}
}

// matchesNoProxy checks if a host matches a NO_PROXY pattern.
// Supports exact matches and wildcard patterns (e.g., "*.example.com").
func matchesNoProxy(host, pattern string) bool {
	// Exact match
	if host == pattern {
		return true
	}

	// Wildcard prefix match (e.g., "*.example.com")
	if strings.HasPrefix(pattern, "*.") {
		domain := pattern[2:]
		return strings.HasSuffix(host, "."+domain) || host == domain
	}

	// Suffix match (e.g., ".example.com")
	if strings.HasPrefix(pattern, ".") {
		return strings.HasSuffix(host, pattern) || host == pattern[1:]
	}

	return false
}

// init ensures the environment cache is loaded before any other package-level initializations.
func init() {
	initEnvCache()
}

// resetEnvCacheForTesting resets the cache and re-initializes it.
// This is only for testing purposes to allow tests to change environment variables.
func resetEnvCacheForTesting() {
	envCacheOnce = sync.Once{}
	envCache.mu.Lock()
	// Zero out all fields
	envCache.port = ""
	envCache.dataDir = ""
	envCache.maxIdleConnsPerHost = 0
	envCache.maxConnsPerHost = 0
	envCache.idleConnTimeout = 0
	envCache.dialTimeout = 0
	envCache.keepAlive = 0
	envCache.fiberPrefork = false
	envCache.fiberConcurrency = 0
	envCache.fiberBodyLimit = 0
	envCache.fiberReadTimeout = 0
	envCache.fiberWriteTimeout = 0
	envCache.fiberIdleTimeout = 0
	envCache.logLevel = ""
	envCache.logFormat = ""
	envCache.asyncLogging = false
	envCache.enableErrorLogging = false
	envCache.asyncLoggingBufferSize = 0
	envCache.otelSDKDisabled = false
	envCache.otelTracesExporter = ""
	envCache.otelDisableTracing = false
	envCache.otelServiceName = ""
	envCache.otelPropagateUpstream = false
	envCache.otelPropagateDownstream = false
	envCache.otelExporterOTLPEndpoint = ""
	envCache.otelExporterOTLPTracesEndpoint = ""
	envCache.enableMetrics = false
	envCache.metricsLabelCacheEnabled = false
	envCache.metricsCacheTTL = 0
	envCache.metricsBatchingEnabled = false
	envCache.metricsBatchSize = 0
	envCache.metricsBatchInterval = 0
	envCache.metricsSampleRate = 0
	envCache.useTraceIDAsRequestID = false
	envCache.healthCheckTimeout = 0
	envCache.healthCheckFailureThreshold = 0
	envCache.readySkipUpstreamCheck = false
	envCache.slowRequestThreshold = 0
	envCache.enableDocs = false
	envCache.maxResponseBodySize = 0
	envCache.httpProxy = nil
	envCache.httpsProxy = nil
	envCache.noProxy = nil
	envCache.proxyFunc = nil
	envCache.metricsBatcherChannelSize = 0
	// Reset OpenFeature state
	envCache.openFeatureEnabled = false
	envCache.openFeatureCacheTTL = 0
	envCache.openFeatureCache.mu.Lock()
	envCache.openFeatureCache.entries = make(map[string]cacheEntry)
	envCache.openFeatureCache.mu.Unlock()
	// Reset global OpenFeature config
	openFeatureCfg.enabled = false
	openFeatureCfg.client = nil
	openFeatureCfg.cacheTTL = 0
	envCache.mu.Unlock()
	initEnvCache()
}
