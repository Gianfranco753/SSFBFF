//go:build goexperiment.jsonv2

// Command server runs the BFF web server. Routes are generated from data/openapi.yaml
// via cmd/apigen. Each route fans out to upstream providers via the aggregator.
//
// Providers are loaded from data/providers/*.yaml at startup.
// The data directory defaults to "data" but can be overridden with DATA_DIR env var.
//
// OpenTelemetry tracing is configured via standard OTEL_* environment variables.
// Set OTEL_SDK_DISABLED=true or OTEL_TRACES_EXPORTER=none to disable tracing.
// See cmd/server/telemetry.go for the full list of supported variables.
//
// Run:
//
//	GOEXPERIMENT=jsonv2 go run ./cmd/server/
package main

import (
	"bytes"
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gcossani/ssfbff/internal/aggregator"
	otelfiber "github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	promhttp "github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/vincentfree/opentelemetry/otelzerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	promexp "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
	"gopkg.in/yaml.v3"
)

// createProviderTransport creates an HTTP transport for a provider with configurable connection pool sizes.
// It respects HTTP_PROXY, HTTPS_PROXY, and NO_PROXY environment variables (cached at startup).
// For high-throughput scenarios, increase MAX_IDLE_CONNS_PER_HOST and MAX_CONNS_PER_HOST.
func createProviderTransport(cfg aggregator.ProviderConfig) *http.Transport {
	maxIdle := cfg.MaxIdleConnsPerHost
	if maxIdle == 0 {
		// Default increased for high concurrency - can be tuned per deployment
		maxIdle = getCachedMaxIdleConnsPerHost()
	}
	maxConns := cfg.MaxConnsPerHost
	if maxConns == 0 {
		// Default increased for high concurrency - can be tuned per deployment
		maxConns = getCachedMaxConnsPerHost()
	}

	// Connection pool tuning for high throughput - use cached values
	idleTimeout := getCachedIdleConnTimeout()
	dialTimeout := getCachedDialTimeout()
	keepAlive := getCachedKeepAlive()

	return &http.Transport{
		Proxy:               getCachedProxyFunc(),
		MaxIdleConnsPerHost: maxIdle,
		MaxConnsPerHost:     maxConns,
		IdleConnTimeout:     idleTimeout,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: keepAlive,
		}).DialContext,
		// Enable connection reuse for better performance
		DisableKeepAlives: false,
	}
}

// createProviderClient creates an HTTP client for a provider with optional OpenTelemetry instrumentation.
// The transport is always wrapped with otelhttp, but tracing behavior is controlled by
// OTEL_DISABLE_TRACING and per-request x-enable-trace header (handled via context).
func createProviderClient(cfg aggregator.ProviderConfig) *http.Client {
	transport := createProviderTransport(cfg)

	// Always wrap with OpenTelemetry instrumentation.
	// The TracerProvider will respect OTEL_DISABLE_TRACING and per-request overrides.
	transport = otelhttp.NewTransport(
		transport,
		otelhttp.WithPropagators(upstreamPropagator()),
	)

	return &http.Client{
		Timeout:   30 * time.Second, // Client-level timeout (should be >= provider timeout)
		Transport: transport,
	}
}

// isTracingEnabledGlobally checks if tracing is enabled globally via cached environment variable.
func isTracingEnabledGlobally() bool {
	return !getCachedOtelDisableTracing()
}

// initLogger configures and returns a zerolog logger with OpenTelemetry trace ID integration.
// It reads LOG_LEVEL (default: info) and LOG_FORMAT (default: json) environment variables (cached at startup).
// The logger is wrapped with otelzerolog to automatically inject trace_id and span_id
// from OpenTelemetry span context when available.
func initLogger() zerolog.Logger {
	levelStr := getCachedLogLevel()
	logLevel, err := zerolog.ParseLevel(levelStr)
	if err != nil {
		logLevel = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(logLevel)

	format := getCachedLogFormat()
	var writer io.Writer = os.Stdout
	if format == "console" {
		writer = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	}

	logger := zerolog.New(writer).With().
		Timestamp().
		Logger()

	logger = otelzerolog.New(logger)

	return logger
}

// serverReady tracks whether the server has finished initialization and is listening.
// It's set to true after the server starts listening in the goroutine.
var (
	serverReady   bool
	serverReadyMu sync.RWMutex
)

// prometheusExporter holds the Prometheus exporter for metrics.
var prometheusExporter *promexp.Exporter

// prometheusRegistry holds the Prometheus registry with the exporter's collector registered.
var prometheusRegistry *prometheus.Registry

// serverAggregator holds the aggregator instance for readiness checks.
var serverAggregator *aggregator.Aggregator

func main() {
	logger := initLogger()

	// Initialize OpenTelemetry first so the instrumented transport and Fiber
	// middleware can register spans under the correct global TracerProvider.
	ctx := context.Background()
	shutdownTracing, err := initTracing(ctx)
	if err != nil {
		logger.Fatal().Err(err).Msg("tracing setup failed")
	}
	defer func() {
		if err := shutdownTracing(ctx); err != nil {
			logger.Error().Err(err).Msg("tracing shutdown failed")
		}
	}()

	// Initialize Prometheus metrics exporter.
	// This creates a meter provider that exports metrics in Prometheus format.
	promExporter, err := promexp.New()
	if err != nil {
		logger.Fatal().Err(err).Msg("prometheus exporter setup failed")
	}
	prometheusExporter = promExporter

	// Custom metrics (http_errors_total, upstream_call_duration_seconds, etc.)
	// are automatically registered to prometheus.DefaultRegisterer via promauto
	// OTel metrics are exported via the Prometheus exporter's meter provider
	// Both will be available through prometheus.DefaultGatherer at /metrics endpoint
	prometheusRegistry = prometheus.DefaultRegisterer.(*prometheus.Registry)

	meterProvider := metric.NewMeterProvider(
		metric.WithReader(promExporter),
	)
	otel.SetMeterProvider(meterProvider)
	defer func() {
		if err := meterProvider.Shutdown(ctx); err != nil {
			logger.Error().Err(err).Msg("meter provider shutdown failed")
		}
		shutdownMetricsBatcher()
	}()

	dataDir := getCachedDataDir()

	providers, err := loadProviders(filepath.Join(dataDir, "providers"))
	if err != nil {
		logger.Fatal().Err(err).Msg("loading providers failed")
	}

	// Create aggregator with per-provider clients (each with its own connection pool)
	// Create aggregator with observability
	obsConfig := &aggregator.ObservabilityConfig{
		Logger:              logger,
		RecordUpstreamCall:  recordUpstreamCall,
		RecordUpstreamError: recordUpstreamError,
		RecordAggregatorOp:  recordAggregatorOperation,
	}
	agg := aggregator.NewWithObservability(providers, createProviderClient, obsConfig)
	serverAggregator = agg

	// Configure Fiber for high performance
	// Prefork can be disabled for single-process deployments (better for containerized environments)
	prefork := getCachedFiberPrefork()
	// Concurrency: higher values allow more concurrent connections per worker
	// Default: 256 * CPU cores (tuned for high throughput)
	concurrency := getCachedFiberConcurrency()
	if concurrency == 0 {
		concurrency = 256 * runtime.NumCPU()
	}
	bodyLimit := getCachedFiberBodyLimit()

	// Timeout configuration - use cached values
	readTimeout := getCachedFiberReadTimeout()
	writeTimeout := getCachedFiberWriteTimeout()
	idleTimeout := getCachedFiberIdleTimeout()

	app := fiber.New(fiber.Config{
		JSONEncoder: func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
		JSONDecoder: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },

		Prefork:           prefork,
		Concurrency:       concurrency,
		BodyLimit:         bodyLimit,
		ReduceMemoryUsage: true,
		DisableKeepalive:  false,

		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	})

	// Add panic recovery middleware first (outermost)
	app.Use(panicRecoveryMiddleware(logger))

	// Instrument all incoming requests with OpenTelemetry.
	// The middleware creates server spans and extracts W3C TraceContext/Baggage.
	// When OTEL_SDK_DISABLED=true, this middleware is skipped entirely.
	// When OTEL_DISABLE_TRACING=true, spans are still created but can be enabled
	// per-request via x-enable-trace header (requires custom sampler implementation).
	if !getCachedOtelSDKDisabled() && getCachedOtelTracesExporter() != "none" {
		app.Use(otelfiber.Middleware(
			otelfiber.WithPropagators(downstreamPropagator()),
		))
	}

	// Extract trace ID from OTel span context and set as X-Request-ID header.
	// This replaces UUID generation since OTel already provides trace IDs.
	// trace_id and span_id are automatically injected into logs via otelzerolog.
	app.Use(traceIDMiddleware())

	// Add error handler middleware
	app.Use(errorHandlerMiddleware(logger))

	// For proxy routes, we still need a client. Use a default one.
	defaultClient := createProviderClient(aggregator.ProviderConfig{})
	RegisterRoutes(app, agg, defaultClient)

	// Set logger for generated routes (SetRouteLogger is generated by apigen)
	// This will be available after running go generate
	// The generated code will call SetRouteLogger if it exists
	setRouteLoggerIfAvailable(logger)

	// Start resource metrics collection (if enabled)
	// Collection interval can be increased via RESOURCE_METRICS_INTERVAL env var (default: 10s)
	resourceMetricsInterval := getCachedResourceMetricsInterval()

	if resourceMetricsEnabled {
		go func() {
			ticker := time.NewTicker(resourceMetricsInterval)
			defer ticker.Stop()
			for range ticker.C {
				updateResourceMetrics()
			}
		}()
	}

	app.Get("/health", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	// Metrics endpoint optimization: use sync.Pool for buffers and optional caching
	var (
		metricsCacheTTL = getCachedMetricsCacheTTL() // seconds, 0 = no cache
		metricsCache    struct {
			mu      sync.RWMutex
			content []byte
			expires time.Time
		}
		bufferPool = sync.Pool{
			New: func() interface{} {
				return &bytes.Buffer{}
			},
		}
	)

	app.Get("/metrics", func(c fiber.Ctx) error {
		// Check cache if enabled
		if metricsCacheTTL > 0 {
			metricsCache.mu.RLock()
			if time.Now().Before(metricsCache.expires) && len(metricsCache.content) > 0 {
				content := metricsCache.content
				metricsCache.mu.RUnlock()
				c.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
				return c.Send(content)
			}
			metricsCache.mu.RUnlock()
		}

		// Gather metrics from default registry (includes promauto custom metrics)
		// OTel metrics are exported via the Prometheus exporter and available through the meter provider
		handler := promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{})

		// Get buffer from pool
		buf := bufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufferPool.Put(buf)

		// Create a mock HTTP request and response writer to capture metrics output.
		req, _ := http.NewRequest("GET", "/metrics", nil)
		handler.ServeHTTP(&mockResponseWriter{Writer: buf}, req)

		content := buf.Bytes()
		contentCopy := make([]byte, len(content))
		copy(contentCopy, content)

		// Update cache if enabled
		if metricsCacheTTL > 0 {
			metricsCache.mu.Lock()
			metricsCache.content = contentCopy
			metricsCache.expires = time.Now().Add(time.Duration(metricsCacheTTL) * time.Second)
			metricsCache.mu.Unlock()
		}

		c.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		return c.Send(contentCopy)
	})

	app.Get("/ready", func(c fiber.Ctx) error {
		serverReadyMu.RLock()
		ready := serverReady && serverAggregator != nil
		serverReadyMu.RUnlock()

		if !ready {
			return c.Status(503).SendString("not ready")
		}

		// Check upstream service availability
		if serverAggregator != nil {
			if !checkUpstreamHealth(serverAggregator) {
				return c.Status(503).SendString("upstream services unavailable")
			}
		}

		return c.SendString("ready")
	})

	app.Get("/live", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	addr := listenAddr()
	logger.Info().Str("address", addr).Msg("BFF server starting")

	go func() {
		if err := app.Listen(addr); err != nil {
			logger.Fatal().Err(err).Msg("server error")
		}
	}()

	// Give the server a moment to start listening, then mark as ready.
	time.Sleep(100 * time.Millisecond)
	serverReadyMu.Lock()
	serverReady = true
	serverReadyMu.Unlock()
	logger.Info().Msg("server ready")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info().Str("signal", sig.String()).Msg("received signal, shutting down")

	if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
		logger.Fatal().Err(err).Msg("shutdown error")
	}
	logger.Info().Msg("server stopped")
}

// loadProviders reads all .yaml files from a directory.
// Each file represents one provider — the filename (minus extension) is the provider name.
func loadProviders(dir string) (map[string]aggregator.ProviderConfig, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading providers dir %s: %w", dir, err)
	}

	providers := make(map[string]aggregator.ProviderConfig, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".yaml")
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}

		var pc aggregator.ProviderConfig
		if err := yaml.Unmarshal(data, &pc); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", entry.Name(), err)
		}
		providers[name] = pc
	}
	return providers, nil
}

func listenAddr() string {
	port := getCachedPort()
	return fmt.Sprintf(":%s", port)
}

// getEnvInt reads an integer environment variable from cache, returning defaultValue if not set or invalid.
// This function is kept for backward compatibility but now uses cached values.
func getEnvInt(key string, defaultValue int) int {
	// Map known keys to cached getters
	switch key {
	case "MAX_IDLE_CONNS_PER_HOST":
		return getCachedMaxIdleConnsPerHost()
	case "MAX_CONNS_PER_HOST":
		return getCachedMaxConnsPerHost()
	case "FIBER_CONCURRENCY":
		return getCachedFiberConcurrency()
	case "FIBER_BODY_LIMIT":
		return getCachedFiberBodyLimit()
	case "METRICS_CACHE_TTL":
		return getCachedMetricsCacheTTL()
	case "ASYNC_LOGGING_BUFFER_SIZE":
		return getCachedAsyncLoggingBufferSize()
	case "METRICS_BATCH_SIZE":
		return getCachedMetricsBatchSize()
	default:
		// Fallback for unknown keys (shouldn't happen in practice)
		return defaultValue
	}
}

// getEnvBool reads a boolean environment variable from cache, returning defaultValue if not set or invalid.
// This function is kept for backward compatibility but now uses cached values.
func getEnvBool(key string, defaultValue bool) bool {
	// Map known keys to cached getters
	switch key {
	case "FIBER_PREFORK":
		return getCachedFiberPrefork()
	case "ASYNC_LOGGING":
		return getCachedAsyncLogging()
	case "ENABLE_ERROR_LOGGING":
		return getCachedEnableErrorLogging()
	case "OTEL_SDK_DISABLED":
		return getCachedOtelSDKDisabled()
	case "OTEL_DISABLE_TRACING":
		return getCachedOtelDisableTracing()
	case "OTEL_PROPAGATE_UPSTREAM":
		return getCachedOtelPropagateUpstream()
	case "OTEL_PROPAGATE_DOWNSTREAM":
		return getCachedOtelPropagateDownstream()
	case "ENABLE_METRICS":
		return getCachedEnableMetrics()
	case "METRICS_LABEL_CACHE_ENABLED":
		return getCachedMetricsLabelCacheEnabled()
	case "ENABLE_RESOURCE_METRICS":
		return getCachedEnableResourceMetrics()
	case "METRICS_BATCHING_ENABLED":
		return getCachedMetricsBatchingEnabled()
	case "USE_TRACE_ID_AS_REQUEST_ID":
		return getCachedUseTraceIDAsRequestID()
	default:
		// Fallback for unknown keys (shouldn't happen in practice)
		return defaultValue
	}
}

// mockResponseWriter implements http.ResponseWriter to capture Prometheus metrics output.
type mockResponseWriter struct {
	io.Writer
	statusCode int
	headers    http.Header
}

func (m *mockResponseWriter) Header() http.Header {
	if m.headers == nil {
		m.headers = make(http.Header)
	}
	return m.headers
}

func (m *mockResponseWriter) Write(b []byte) (int, error) {
	return m.Writer.Write(b)
}

func (m *mockResponseWriter) WriteHeader(statusCode int) {
	m.statusCode = statusCode
}
