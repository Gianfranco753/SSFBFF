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
	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	promhttp "github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
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
	baseTransport := createProviderTransport(cfg)

	// Always wrap with OpenTelemetry instrumentation.
	// The TracerProvider will respect OTEL_DISABLE_TRACING and per-request overrides.
	instrumentedTransport := otelhttp.NewTransport(
		baseTransport,
		otelhttp.WithPropagators(upstreamPropagator()),
	)

	return &http.Client{
		Timeout:   30 * time.Second, // Client-level timeout (should be >= provider timeout)
		Transport: instrumentedTransport,
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

	// otelzerolog works by decorating log events with AddTracingContext
	// The logger itself doesn't need to be wrapped - tracing context is added
	// when logging via middleware or explicit AddTracingContext calls
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
	shutdownTracing, err := initTracing(ctx, logger)
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

	// Register Prometheus Go collector for built-in Go runtime metrics
	// This provides go_goroutines, go_memstats_*, and other Go runtime metrics
	// Use Register instead of MustRegister to avoid panic if already registered
	// (e.g., by the OpenTelemetry Prometheus exporter)
	if err := prometheusRegistry.Register(collectors.NewGoCollector()); err != nil {
		// Collector may already be registered, which is fine - ignore duplicate registration errors
		errStr := err.Error()
		if !strings.Contains(errStr, "duplicate") && !strings.Contains(errStr, "already registered") {
			logger.Warn().Err(err).Msg("failed to register Go collector")
		}
	}

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
	// Prefork: disabled by default for containerized deployments (Docker/Kubernetes).
	// In containerized environments, scale horizontally (multiple containers) rather than
	// vertically (multiple processes per container). Enable prefork only if you have
	// multiple dedicated CPU cores per container and aren't using an orchestrator.
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

	// Pre-allocate common error response templates to avoid allocations
	errorResponsePool := sync.Pool{
		New: func() interface{} {
			return &bytes.Buffer{}
		},
	}
	
	app := fiber.New(fiber.Config{
		JSONEncoder: func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
		JSONDecoder: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },
		ErrorHandler: func(c fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			message := "Internal Server Error"

			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
				message = e.Message
			} else {
				message = err.Error()
			}

			// Use sync.Pool buffer to avoid fiber.Map allocation
			buf := errorResponsePool.Get().(*bytes.Buffer)
			buf.Reset()
			defer errorResponsePool.Put(buf)
			
			// Build JSON response directly
			buf.WriteString(`{"error":`)
			jsonBytes, _ := jsonv2.Marshal(message)
			buf.Write(jsonBytes)
			buf.WriteString(`,"status":`)
			buf.WriteString(strconv.Itoa(code))
			buf.WriteString(`}`)
			
			c.Set("Content-Type", "application/json")
			return c.Status(code).Send(buf.Bytes())
		},

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

	// Combined OpenTelemetry instrumentation and trace ID extraction.
	// This middleware creates server spans, extracts W3C TraceContext/Baggage,
	// and sets X-Request-ID header from the trace ID.
	// When OTEL_SDK_DISABLED=true or OTEL_TRACES_EXPORTER=none, this is a no-op.
	// When OTEL_DISABLE_TRACING=true, spans are still created but can be enabled
	// per-request via x-enable-trace header (requires custom sampler implementation).
	app.Use(otelWithTraceIDMiddleware())

	// Add error handler middleware
	app.Use(errorHandlerMiddleware(logger))

	// For proxy routes, we still need a client. Use a default one.
	defaultClient := createProviderClient(aggregator.ProviderConfig{})
	RegisterRoutes(app, agg, defaultClient)

	// Set logger for generated routes (SetRouteLogger is generated by apigen)
	// This will be available after running go generate
	// The generated code will call SetRouteLogger if it exists
	setRouteLoggerIfAvailable(logger)

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
			return c.Status(503).JSON(fiber.Map{
				"healthy": false,
				"reason":  "server not ready",
			})
		}

		// Check upstream service availability
		startTime := time.Now()
		healthStatus := checkUpstreamHealth(serverAggregator)
		duration := time.Since(startTime)
		recordHealthCheckDuration(duration)

		if !healthStatus.Healthy {
			return c.Status(503).JSON(healthStatus)
		}

		return c.JSON(healthStatus)
	})

	app.Get("/live", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	// Scalar documentation endpoint (enabled via ENABLE_DOCS env var)
	if getCachedEnableDocs() {
		app.Get("/docs", func(c fiber.Ctx) error {
			openAPIPath := filepath.Join(dataDir, "openapi.yaml")
			specData, err := os.ReadFile(openAPIPath)
			if err != nil {
				logger.Error().Err(err).Str("path", openAPIPath).Msg("failed to read OpenAPI spec")
				return c.Status(500).JSON(fiber.Map{
					"error": "failed to load OpenAPI specification",
				})
			}

			// Convert YAML to JSON for Scalar
			var specObj interface{}
			if err := yaml.Unmarshal(specData, &specObj); err != nil {
				logger.Error().Err(err).Msg("failed to parse OpenAPI spec")
				return c.Status(500).JSON(fiber.Map{
					"error": "failed to parse OpenAPI specification",
				})
			}

			specJSON, err := jsonv2.Marshal(specObj)
			if err != nil {
				logger.Error().Err(err).Msg("failed to marshal OpenAPI spec to JSON")
				return c.Status(500).JSON(fiber.Map{
					"error": "failed to convert OpenAPI specification",
				})
			}

			// Escape JSON for embedding in HTML script tag
			specJSONStr := string(specJSON)
			specJSONEscaped := strings.ReplaceAll(specJSONStr, "</script>", "<\\/script>")

			html := `<!doctype html>
<html>
  <head>
    <title>API Documentation</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <style>
      body {
        margin: 0;
      }
    </style>
  </head>
  <body>
    <script id="api-reference" type="application/json">` + specJSONEscaped + `</script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@latest/dist/browser/standalone.js"></script>
  </body>
</html>`

			c.Set("Content-Type", "text/html; charset=utf-8")
			return c.SendString(html)
		})
	}

	addr := listenAddr()
	logger.Info().Str("address", addr).Msg("BFF server starting")

	// When prefork is enabled, app.Listen() manages child processes.
	// The parent process blocks to manage children, so we run it in a goroutine
	// to allow the main function to handle shutdown signals.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error().
					Interface("panic", r).
					Msg("panic in server goroutine")
				os.Exit(1)
			}
		}()

		listenConfig := fiber.ListenConfig{
			EnablePrefork: prefork,
		}
		if err := app.Listen(addr, listenConfig); err != nil {
			// In prefork mode, errors from child processes are handled by Fiber.
			// This error typically indicates the parent process failed to start or manage children.
			logger.Error().
				Err(err).
				Bool("prefork", prefork).
				Str("address", addr).
				Msg("server listen error")
			os.Exit(1)
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

	shutdownStart := time.Now()
	shutdownTimeout := getCachedShutdownTimeout()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	// Step 1: Stop accepting new requests (Fiber shutdown)
	logger.Info().Msg("stopping HTTP server")
	if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
		logger.Error().Err(err).Msg("HTTP server shutdown error")
	}

	// Step 2: Wait for in-flight aggregator requests
	// The aggregator uses context cancellation, so in-flight requests will be cancelled
	// when shutdownCtx is cancelled. We give them a moment to finish.
	logger.Info().Msg("waiting for in-flight requests")
	select {
	case <-shutdownCtx.Done():
		logger.Warn().Msg("shutdown timeout reached while waiting for in-flight requests")
	case <-time.After(1 * time.Second):
		// Brief pause for in-flight requests to complete
	}

	// Step 3: Drain metrics batcher
	logger.Info().Msg("draining metrics batcher")
	shutdownMetricsBatcher()

	// Step 4: Shutdown async logging worker
	logger.Info().Msg("shutting down async logging worker")
	asyncLogTimeout := 5 * time.Second
	if !shutdownAsyncLogging(asyncLogTimeout) {
		logger.Warn().Msg("async logging worker did not finish in time")
	}

	// Step 5 & 6: OpenTelemetry and metrics shutdown are handled by defer functions
	// They will be called when we return from main

	shutdownDuration := time.Since(shutdownStart)
	recordShutdownDuration(shutdownDuration)
	logger.Info().Dur("duration", shutdownDuration).Msg("server stopped")
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

		// Validate provider configuration
		if err := aggregator.ValidateProviderConfig(name, pc); err != nil {
			return nil, fmt.Errorf("invalid configuration in %s: %w", entry.Name(), err)
		}

		providers[name] = pc
	}
	return providers, nil
}

func listenAddr() string {
	port := getCachedPort()
	return fmt.Sprintf(":%s", port)
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
