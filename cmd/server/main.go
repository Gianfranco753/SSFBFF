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
// It respects HTTP_PROXY, HTTPS_PROXY, and NO_PROXY environment variables.
// For high-throughput scenarios, increase MAX_IDLE_CONNS_PER_HOST and MAX_CONNS_PER_HOST.
func createProviderTransport(cfg aggregator.ProviderConfig) *http.Transport {
	maxIdle := cfg.MaxIdleConnsPerHost
	if maxIdle == 0 {
		// Default increased for high concurrency - can be tuned per deployment
		maxIdle = getEnvInt("MAX_IDLE_CONNS_PER_HOST", 2000)
	}
	maxConns := cfg.MaxConnsPerHost
	if maxConns == 0 {
		// Default increased for high concurrency - can be tuned per deployment
		maxConns = getEnvInt("MAX_CONNS_PER_HOST", 5000)
	}

	// Connection pool tuning for high throughput
	idleTimeout := 90 * time.Second
	if timeoutStr := os.Getenv("IDLE_CONN_TIMEOUT"); timeoutStr != "" {
		if parsed, err := time.ParseDuration(timeoutStr); err == nil && parsed > 0 {
			idleTimeout = parsed
		}
	}

	dialTimeout := 3 * time.Second
	if timeoutStr := os.Getenv("DIAL_TIMEOUT"); timeoutStr != "" {
		if parsed, err := time.ParseDuration(timeoutStr); err == nil && parsed > 0 {
			dialTimeout = parsed
		}
	}

	keepAlive := 30 * time.Second
	if keepAliveStr := os.Getenv("KEEP_ALIVE"); keepAliveStr != "" {
		if parsed, err := time.ParseDuration(keepAliveStr); err == nil && parsed > 0 {
			keepAlive = parsed
		}
	}

	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
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

// isTracingEnabledGlobally checks if tracing is enabled globally via environment variable.
func isTracingEnabledGlobally() bool {
	return os.Getenv("OTEL_DISABLE_TRACING") != "true"
}

// initLogger configures and returns a zerolog logger with OpenTelemetry trace ID integration.
// It reads LOG_LEVEL (default: info) and LOG_FORMAT (default: json) environment variables.
// The logger is wrapped with otelzerolog to automatically inject trace_id and span_id
// from OpenTelemetry span context when available.
func initLogger() zerolog.Logger {
	levelStr := os.Getenv("LOG_LEVEL")
	if levelStr == "" {
		levelStr = "info"
	}
	logLevel, err := zerolog.ParseLevel(levelStr)
	if err != nil {
		logLevel = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(logLevel)

	format := os.Getenv("LOG_FORMAT")
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

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}

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
	prefork := getEnvBool("FIBER_PREFORK", true)
	// Concurrency: higher values allow more concurrent connections per worker
	// Default: 256 * CPU cores (tuned for high throughput)
	concurrency := getEnvInt("FIBER_CONCURRENCY", 256*runtime.NumCPU())
	bodyLimit := getEnvInt("FIBER_BODY_LIMIT", 10*1024*1024) // 10MB default

	// Timeout configuration - can be tuned for high-throughput scenarios
	readTimeout := 5 * time.Second
	if timeoutStr := os.Getenv("FIBER_READ_TIMEOUT"); timeoutStr != "" {
		if parsed, err := time.ParseDuration(timeoutStr); err == nil && parsed > 0 {
			readTimeout = parsed
		}
	}

	writeTimeout := 10 * time.Second
	if timeoutStr := os.Getenv("FIBER_WRITE_TIMEOUT"); timeoutStr != "" {
		if parsed, err := time.ParseDuration(timeoutStr); err == nil && parsed > 0 {
			writeTimeout = parsed
		}
	}

	idleTimeout := 120 * time.Second
	if timeoutStr := os.Getenv("FIBER_IDLE_TIMEOUT"); timeoutStr != "" {
		if parsed, err := time.ParseDuration(timeoutStr); err == nil && parsed > 0 {
			idleTimeout = parsed
		}
	}

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
	if os.Getenv("OTEL_SDK_DISABLED") != "true" && os.Getenv("OTEL_TRACES_EXPORTER") != "none" {
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
	resourceMetricsInterval := 10 * time.Second
	if intervalStr := os.Getenv("RESOURCE_METRICS_INTERVAL"); intervalStr != "" {
		if parsed, err := time.ParseDuration(intervalStr); err == nil && parsed > 0 {
			resourceMetricsInterval = parsed
		}
	}

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
		metricsCacheTTL = getEnvInt("METRICS_CACHE_TTL", 0) // seconds, 0 = no cache
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
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	return fmt.Sprintf(":%s", port)
}

// getEnvInt reads an integer environment variable, returning defaultValue if not set or invalid.
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
