//go:build goexperiment.jsonv2

package main

import (
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
	port                    string
	dataDir                 string
	maxIdleConnsPerHost     int
	maxConnsPerHost         int
	idleConnTimeout         time.Duration
	dialTimeout             time.Duration
	keepAlive               time.Duration

	// Fiber configuration
	fiberPrefork            bool
	fiberConcurrency        int
	fiberBodyLimit          int
	fiberReadTimeout        time.Duration
	fiberWriteTimeout       time.Duration
	fiberIdleTimeout        time.Duration

	// Logging configuration
	logLevel                string
	logFormat               string
	asyncLogging            bool
	enableErrorLogging      bool
	asyncLoggingBufferSize  int

	// OpenTelemetry configuration
	otelSDKDisabled         bool
	otelTracesExporter      string
	otelDisableTracing       bool
	otelServiceName          string
	otelPropagateUpstream    bool
	otelPropagateDownstream  bool

	// Metrics configuration
	enableMetrics            bool
	metricsLabelCacheEnabled bool
	enableResourceMetrics    bool
	resourceMetricsInterval  time.Duration
	metricsCacheTTL          int
	metricsBatchingEnabled   bool
	metricsBatchSize         int
	metricsBatchInterval     time.Duration
	metricsSampleRate        float64

	// Middleware configuration
	useTraceIDAsRequestID    bool

	// Proxy configuration (cached to avoid http.ProxyFromEnvironment overhead)
	httpProxy                *url.URL
	httpsProxy               *url.URL
	noProxy                  []string
	proxyFunc                func(*http.Request) (*url.URL, error)
	}
	envCacheOnce sync.Once
)

// initEnvCache loads all environment variables into the cache at startup.
// This is called via init() to ensure it runs before any package-level variable initializations.
func initEnvCache() {
	envCache.mu.Lock()
	defer envCache.mu.Unlock()

	// Server configuration
	envCache.port = getEnvString("PORT", "3000")
	envCache.dataDir = getEnvString("DATA_DIR", "data")
	envCache.maxIdleConnsPerHost = getEnvInt("MAX_IDLE_CONNS_PER_HOST", 2000)
	envCache.maxConnsPerHost = getEnvInt("MAX_CONNS_PER_HOST", 5000)
	envCache.idleConnTimeout = getEnvDuration("IDLE_CONN_TIMEOUT", 90*time.Second)
	envCache.dialTimeout = getEnvDuration("DIAL_TIMEOUT", 3*time.Second)
	envCache.keepAlive = getEnvDuration("KEEP_ALIVE", 30*time.Second)

	// Fiber configuration
	envCache.fiberPrefork = getEnvBool("FIBER_PREFORK", true)
	envCache.fiberConcurrency = getEnvInt("FIBER_CONCURRENCY", 0) // 0 means use default calculation
	envCache.fiberBodyLimit = getEnvInt("FIBER_BODY_LIMIT", 10*1024*1024)
	envCache.fiberReadTimeout = getEnvDuration("FIBER_READ_TIMEOUT", 5*time.Second)
	envCache.fiberWriteTimeout = getEnvDuration("FIBER_WRITE_TIMEOUT", 10*time.Second)
	envCache.fiberIdleTimeout = getEnvDuration("FIBER_IDLE_TIMEOUT", 120*time.Second)

	// Logging configuration
	envCache.logLevel = getEnvString("LOG_LEVEL", "info")
	envCache.logFormat = getEnvString("LOG_FORMAT", "json")
	envCache.asyncLogging = getEnvBool("ASYNC_LOGGING", false)
	envCache.enableErrorLogging = getEnvBool("ENABLE_ERROR_LOGGING", true)
	envCache.asyncLoggingBufferSize = getEnvInt("ASYNC_LOGGING_BUFFER_SIZE", 1000)

	// OpenTelemetry configuration
	envCache.otelSDKDisabled = getEnvBool("OTEL_SDK_DISABLED", false)
	envCache.otelTracesExporter = getEnvString("OTEL_TRACES_EXPORTER", "")
	envCache.otelDisableTracing = getEnvBool("OTEL_DISABLE_TRACING", false)
	envCache.otelServiceName = getEnvString("OTEL_SERVICE_NAME", "ssfbff")
	envCache.otelPropagateUpstream = getEnvBool("OTEL_PROPAGATE_UPSTREAM", true)
	envCache.otelPropagateDownstream = getEnvBool("OTEL_PROPAGATE_DOWNSTREAM", true)

	// Metrics configuration
	envCache.enableMetrics = getEnvBool("ENABLE_METRICS", true)
	envCache.metricsLabelCacheEnabled = getEnvBool("METRICS_LABEL_CACHE_ENABLED", true)
	envCache.enableResourceMetrics = getEnvBool("ENABLE_RESOURCE_METRICS", true)
	envCache.resourceMetricsInterval = getEnvDuration("RESOURCE_METRICS_INTERVAL", 10*time.Second)
	envCache.metricsCacheTTL = getEnvInt("METRICS_CACHE_TTL", 0)
	envCache.metricsBatchingEnabled = getEnvBool("METRICS_BATCHING_ENABLED", true)
	envCache.metricsBatchSize = getEnvInt("METRICS_BATCH_SIZE", 1000)
	envCache.metricsBatchInterval = getEnvDuration("METRICS_BATCH_INTERVAL", 100*time.Millisecond)
	envCache.metricsSampleRate = getEnvFloat("METRICS_SAMPLE_RATE", 1.0)

	// Middleware configuration
	envCache.useTraceIDAsRequestID = getEnvBool("USE_TRACE_ID_AS_REQUEST_ID", true)

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

// Helper functions to get cached values (thread-safe)

func getCachedPort() string {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.port
}

func getCachedDataDir() string {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.dataDir
}

func getCachedMaxIdleConnsPerHost() int {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.maxIdleConnsPerHost
}

func getCachedMaxConnsPerHost() int {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.maxConnsPerHost
}

func getCachedIdleConnTimeout() time.Duration {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.idleConnTimeout
}

func getCachedDialTimeout() time.Duration {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.dialTimeout
}

func getCachedKeepAlive() time.Duration {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.keepAlive
}

func getCachedFiberPrefork() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.fiberPrefork
}

func getCachedFiberConcurrency() int {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.fiberConcurrency
}

func getCachedFiberBodyLimit() int {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.fiberBodyLimit
}

func getCachedFiberReadTimeout() time.Duration {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.fiberReadTimeout
}

func getCachedFiberWriteTimeout() time.Duration {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.fiberWriteTimeout
}

func getCachedFiberIdleTimeout() time.Duration {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.fiberIdleTimeout
}

func getCachedLogLevel() string {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.logLevel
}

func getCachedLogFormat() string {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.logFormat
}

func getCachedAsyncLogging() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.asyncLogging
}

func getCachedEnableErrorLogging() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.enableErrorLogging
}

func getCachedAsyncLoggingBufferSize() int {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.asyncLoggingBufferSize
}

func getCachedOtelSDKDisabled() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.otelSDKDisabled
}

func getCachedOtelTracesExporter() string {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.otelTracesExporter
}

func getCachedOtelDisableTracing() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.otelDisableTracing
}

func getCachedOtelServiceName() string {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.otelServiceName
}

func getCachedOtelPropagateUpstream() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.otelPropagateUpstream
}

func getCachedOtelPropagateDownstream() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.otelPropagateDownstream
}

func getCachedEnableMetrics() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.enableMetrics
}

func getCachedMetricsLabelCacheEnabled() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.metricsLabelCacheEnabled
}

func getCachedEnableResourceMetrics() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.enableResourceMetrics
}

func getCachedResourceMetricsInterval() time.Duration {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.resourceMetricsInterval
}

func getCachedMetricsCacheTTL() int {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.metricsCacheTTL
}

func getCachedMetricsBatchingEnabled() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.metricsBatchingEnabled
}

func getCachedMetricsBatchSize() int {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.metricsBatchSize
}

func getCachedMetricsBatchInterval() time.Duration {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.metricsBatchInterval
}

func getCachedMetricsSampleRate() float64 {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.metricsSampleRate
}

func getCachedUseTraceIDAsRequestID() bool {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
	return envCache.useTraceIDAsRequestID
}

func getCachedProxyFunc() func(*http.Request) (*url.URL, error) {
	ensureCacheInitialized()
	envCache.mu.RLock()
	defer envCache.mu.RUnlock()
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
	envCache.enableMetrics = false
	envCache.metricsLabelCacheEnabled = false
	envCache.enableResourceMetrics = false
	envCache.resourceMetricsInterval = 0
	envCache.metricsCacheTTL = 0
	envCache.metricsBatchingEnabled = false
	envCache.metricsBatchSize = 0
	envCache.metricsBatchInterval = 0
	envCache.metricsSampleRate = 0
	envCache.useTraceIDAsRequestID = false
	envCache.httpProxy = nil
	envCache.httpsProxy = nil
	envCache.noProxy = nil
	envCache.proxyFunc = nil
	envCache.mu.Unlock()
	initEnvCache()
}
