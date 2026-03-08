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
	"encoding/json/jsontext"
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
	rt "github.com/gcossani/ssfbff/runtime"
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

// initLogger configures and returns a zerolog logger.
// It reads LOG_LEVEL (default: info) and LOG_FORMAT (default: json) environment variables (cached at startup).
// Trace IDs are manually injected in the async logging worker from the stored context.
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

	// Trace IDs are manually injected in async logging worker
	// No hook needed since all logging is async
	return logger
}

// errorCodeFromStatus maps HTTP status to API error code for fiber.Error responses.
func errorCodeFromStatus(status int) string {
	switch {
	case status == 400:
		return rt.ErrorCodeValidationError
	case status == 404:
		return rt.ErrorCodeInvalidRequest
	case status == 502:
		return rt.ErrorCodeBadGateway
	case status == 503:
		return rt.ErrorCodeUpstreamUnavailable
	default:
		return rt.ErrorCodeInternalError
	}
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

// inFlightWg counts requests currently being handled so shutdown can wait for them to drain.
var inFlightWg sync.WaitGroup

// cachedOpenAPISpecHTML holds the cached HTML for the /docs endpoint.
var (
	cachedOpenAPISpecHTML []byte
	cachedOpenAPISpecOnce sync.Once
)

func main() {
	logger := initLogger()

	// Initialize async logging worker early so all logging is asynchronous
	initAsyncLogging()

	// Initialize OpenTelemetry first so the instrumented transport and Fiber
	// middleware can register spans under the correct global TracerProvider.
	ctx := context.Background()
	shutdownTracing, err := initTracing(ctx, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("tracing setup failed")
	}
	defer func() {
		if err := shutdownTracing(ctx); err != nil {
			logError(ctx, logger, "tracing shutdown failed", func(e *zerolog.Event) { e.Err(err) })
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
			logWarn(ctx, logger, "failed to register Go collector", func(e *zerolog.Event) { e.Err(err) })
		}
	}

	meterProvider := metric.NewMeterProvider(
		metric.WithReader(promExporter),
	)
	otel.SetMeterProvider(meterProvider)
	defer func() {
		if err := meterProvider.Shutdown(ctx); err != nil {
			logError(ctx, logger, "meter provider shutdown failed", func(e *zerolog.Event) { e.Err(err) })
		}
		shutdownMetricsBatcher()
	}()

	dataDir := getCachedDataDir()

	// Load and cache OpenAPI spec HTML if docs are enabled
	if getCachedEnableDocs() {
		var loadErr error
		cachedOpenAPISpecOnce.Do(func() {
			cachedOpenAPISpecHTML, loadErr = loadOpenAPISpecHTML(dataDir)
			if loadErr != nil {
				logError(ctx, logger, "failed to load OpenAPI spec for docs",
					func(e *zerolog.Event) { e.Err(loadErr) })
			}
		})
	}

	providers, err := loadProviders(filepath.Join(dataDir, "providers"))
	if err != nil {
		logger.Fatal().Err(err).Msg("loading providers failed")
	}

	// Create aggregator with per-provider clients (each with its own connection pool)
	// Create aggregator with observability
	obsConfig := &aggregator.ObservabilityConfig{
		Logger: logger,
		LogFunc: func(ctx context.Context, level zerolog.Level, msg string, fields ...func(*zerolog.Event)) {
			buildEvent := func(l zerolog.Logger) *zerolog.Event {
				var event *zerolog.Event
				switch level {
				case zerolog.DebugLevel:
					event = l.Debug()
				case zerolog.InfoLevel:
					event = l.Info()
				case zerolog.WarnLevel:
					event = l.Warn()
				case zerolog.ErrorLevel:
					event = l.Error()
				case zerolog.FatalLevel:
					event = l.Fatal()
				case zerolog.PanicLevel:
					event = l.Panic()
				default:
					event = l.Info()
				}
				for _, f := range fields {
					f(event)
				}
				return event
			}
			logAsync(level, buildEvent, msg, ctx, logger)
		},
		RecordUpstreamCall:  recordUpstreamCall,
		RecordUpstreamError: recordUpstreamError,
		RecordAggregatorOp:  recordAggregatorOperation,
	}
	maxResponseBodySize := getCachedMaxResponseBodySize()
	agg := aggregator.NewWithObservability(providers, createProviderClient, obsConfig, maxResponseBodySize)
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
			errorCode := rt.ErrorCodeInternalError

			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
				message = e.Message
				errorCode = errorCodeFromStatus(code)
			} else {
				// Sanitize error message and classify error code
				message = rt.SanitizeError(err)
				errorCode = rt.ClassifyError(err)
			}

			// Use sync.Pool buffer to avoid fiber.Map allocation
			buf := errorResponsePool.Get().(*bytes.Buffer)
			buf.Reset()
			defer errorResponsePool.Put(buf)

			// Build JSON response using jsontext.Encoder for consistent, efficient encoding
			enc := jsontext.NewEncoder(buf)
			enc.WriteToken(jsontext.BeginObject)
			enc.WriteToken(jsontext.String("error"))
			enc.WriteToken(jsontext.String(message))
			enc.WriteToken(jsontext.String("status"))
			enc.WriteToken(jsontext.String(strconv.Itoa(code)))
			enc.WriteToken(jsontext.String("code"))
			enc.WriteToken(jsontext.String(errorCode))
			enc.WriteToken(jsontext.EndObject)

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

	// In-flight tracking first (outermost) so every request is counted until handler returns.
	app.Use(func(c fiber.Ctx) error {
		inFlightWg.Add(1)
		defer inFlightWg.Done()
		return c.Next()
	})
	// Panic recovery so recovered handlers still decrement in-flight when they return.
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

	// Register built-in routes first so they take precedence over generated wildcards (e.g. /proxy/*).
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

		// Update cache if enabled (only copy when caching is needed)
		if metricsCacheTTL > 0 {
			contentCopy := make([]byte, len(content))
			copy(contentCopy, content)
			metricsCache.mu.Lock()
			metricsCache.content = contentCopy
			metricsCache.expires = time.Now().Add(time.Duration(metricsCacheTTL) * time.Second)
			metricsCache.mu.Unlock()
			c.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			return c.Send(contentCopy)
		}

		c.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		return c.Send(content)
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

		// When READY_SKIP_UPSTREAM_CHECK=true (e.g. local dev), report ready without probing upstreams.
		if getCachedReadySkipUpstreamCheck() {
			return c.JSON(fiber.Map{
				"healthy": true,
				"reason":  "READY_SKIP_UPSTREAM_CHECK enabled",
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
	// Uses cached HTML loaded at startup to avoid file I/O on every request
	if getCachedEnableDocs() {
		app.Get("/docs", func(c fiber.Ctx) error {
			if len(cachedOpenAPISpecHTML) == 0 {
				logError(c.Context(), logger, "OpenAPI spec HTML not loaded",
					func(e *zerolog.Event) {})
				return c.Status(500).JSON(fiber.Map{
					"error": "API documentation not available",
				})
			}
			c.Set("Content-Type", "text/html; charset=utf-8")
			return c.Send(cachedOpenAPISpecHTML)
		})
	}

	// OpenAPI and proxy routes (generated). For proxy routes we use a default client.
	defaultClient := createProviderClient(aggregator.ProviderConfig{})
	RegisterRoutes(app, agg, defaultClient)

	addr := listenAddr()
	logInfo(ctx, logger, "BFF server starting", func(e *zerolog.Event) { e.Str("address", addr) })

	// When prefork is enabled, app.Listen() manages child processes.
	// The parent process blocks to manage children, so we run it in a goroutine
	// to allow the main function to handle shutdown signals.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logError(ctx, logger, "panic in server goroutine",
					func(e *zerolog.Event) { e.Interface("panic", r) })
				os.Exit(1)
			}
		}()

		listenConfig := fiber.ListenConfig{
			EnablePrefork: prefork,
		}
		if err := app.Listen(addr, listenConfig); err != nil {
			// In prefork mode, errors from child processes are handled by Fiber.
			// This error typically indicates the parent process failed to start or manage children.
			logError(ctx, logger, "server listen error",
				func(e *zerolog.Event) {
					e.Err(err).Bool("prefork", prefork).Str("address", addr)
				})
			os.Exit(1)
		}
	}()

	// Give the server a moment to start listening, then mark as ready.
	time.Sleep(100 * time.Millisecond)
	serverReadyMu.Lock()
	serverReady = true
	serverReadyMu.Unlock()
	logInfo(ctx, logger, "server ready")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logInfo(ctx, logger, "received signal, shutting down", func(e *zerolog.Event) { e.Str("signal", sig.String()) })

	shutdownStart := time.Now()
	shutdownTimeout := getCachedShutdownTimeout()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	// Step 1: Stop accepting new requests (Fiber shutdown)
	logInfo(shutdownCtx, logger, "stopping HTTP server")
	// Calculate Fiber shutdown timeout (80% of total, min 5s, max remaining)
	fiberShutdownTimeout := shutdownTimeout * 80 / 100
	minShutdownTimeout := 5 * time.Second
	if fiberShutdownTimeout < minShutdownTimeout {
		fiberShutdownTimeout = minShutdownTimeout
	}
	deadline, hasDeadline := shutdownCtx.Deadline()
	if hasDeadline {
		remaining := time.Until(deadline)
		hasRemainingTime := remaining > 0
		exceedsRemaining := fiberShutdownTimeout > remaining
		if hasRemainingTime && exceedsRemaining {
			fiberShutdownTimeout = remaining
		}
	}
	if err := app.ShutdownWithTimeout(fiberShutdownTimeout); err != nil {
		logError(shutdownCtx, logger, "HTTP server shutdown error", func(e *zerolog.Event) { e.Err(err) })
	}

	// Step 2: Wait for in-flight requests to drain (exit as soon as count hits zero, or after timeout).
	logInfo(shutdownCtx, logger, "waiting for in-flight requests")
	maxWaitTime := 5 * time.Second
	defaultWaitTime := 1 * time.Second

	deadline2, hasDeadline2 := shutdownCtx.Deadline()
	var waitTime time.Duration
	if hasDeadline2 {
		remainingTime := time.Until(deadline2)
		hasRemainingTime := remainingTime > 0
		if hasRemainingTime {
			waitTime = remainingTime
			if waitTime > maxWaitTime {
				waitTime = maxWaitTime
			}
		} else {
			// Shutdown context already expired
			logWarn(shutdownCtx, logger, "shutdown timeout reached while waiting for in-flight requests")
			waitTime = 0 // Don't wait if already expired
		}
	} else {
		// No deadline set (shouldn't happen with WithTimeout, but handle gracefully)
		waitTime = defaultWaitTime
	}

	if waitTime > 0 {
		completed := waitInFlightWithTimeout(shutdownCtx, &inFlightWg, waitTime)
		if !completed {
			logWarn(shutdownCtx, logger, "shutdown timeout reached while waiting for in-flight requests")
		}
	}

	// Step 3: Drain metrics batcher
	logInfo(shutdownCtx, logger, "draining metrics batcher")
	shutdownMetricsBatcher()

	// Step 4: Shutdown async logging worker
	logInfo(shutdownCtx, logger, "shutting down async logging worker")
	asyncLogTimeout := 5 * time.Second
	if !shutdownAsyncLogging(asyncLogTimeout) {
		logWarn(shutdownCtx, logger, "async logging worker did not finish in time")
	}

	// Step 5 & 6: OpenTelemetry and metrics shutdown are handled by defer functions
	// They will be called when we return from main

	shutdownDuration := time.Since(shutdownStart)
	recordShutdownDuration(shutdownDuration)
	logInfo(shutdownCtx, logger, "server stopped", func(e *zerolog.Event) { e.Dur("duration", shutdownDuration) })
}

// waitInFlightWithTimeout waits for wg to reach zero or for timeout/context cancellation.
// Returns true if the WaitGroup completed first, false if timeout or context cancelled.
func waitInFlightWithTimeout(ctx context.Context, wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	case <-ctx.Done():
		return false
	}
}

// loadOpenAPISpecHTML loads the OpenAPI spec, converts it to JSON, and builds the HTML template.
// This is called once at startup and cached for the /docs endpoint.
func loadOpenAPISpecHTML(dataDir string) ([]byte, error) {
	openAPIPath := filepath.Join(dataDir, "openapi.yaml")
	specData, err := os.ReadFile(openAPIPath)
	if err != nil {
		return nil, fmt.Errorf("reading OpenAPI spec: %w", err)
	}

	// Convert YAML to JSON for Scalar
	var specObj interface{}
	if err := yaml.Unmarshal(specData, &specObj); err != nil {
		return nil, fmt.Errorf("parsing OpenAPI spec: %w", err)
	}

	specJSON, err := jsonv2.Marshal(specObj)
	if err != nil {
		return nil, fmt.Errorf("marshaling OpenAPI spec to JSON: %w", err)
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

	return []byte(html), nil
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
