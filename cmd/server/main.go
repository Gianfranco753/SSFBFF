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

// baseTransport defines connection parameters for all upstream HTTP calls.
// It respects HTTP_PROXY, HTTPS_PROXY, and NO_PROXY environment variables.
var baseTransport = &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	MaxIdleConnsPerHost: 64,
	MaxConnsPerHost:     128,
	IdleConnTimeout:     90 * time.Second,
	DialContext: (&net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
}

// sharedHTTPClient is used by the aggregator for all upstream calls.
// In main(), after OTel is initialized, its Transport is replaced with an
// otelhttp-wrapped version so every upstream call gets a trace span.
var sharedHTTPClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: baseTransport,
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

	// Register the exporter's collector with a Prometheus registry.
	prometheusRegistry = prometheus.NewRegistry()
	if err := prometheusRegistry.Register(promExporter.Collector); err != nil {
		logger.Fatal().Err(err).Msg("registering prometheus collector failed")
	}

	meterProvider := metric.NewMeterProvider(
		metric.WithReader(promExporter),
	)
	otel.SetMeterProvider(meterProvider)
	defer func() {
		if err := meterProvider.Shutdown(ctx); err != nil {
			logger.Error().Err(err).Msg("meter provider shutdown failed")
		}
	}()

	// Wrap baseTransport with otelhttp so all upstream HTTP calls become child
	// spans of the active trace. upstreamPropagator() controls whether
	// traceparent/tracestate headers are injected into those outgoing requests
	// (OTEL_PROPAGATE_UPSTREAM, default: true).
	sharedHTTPClient.Transport = otelhttp.NewTransport(
		baseTransport,
		otelhttp.WithPropagators(upstreamPropagator()),
	)

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}

	providers, err := loadProviders(filepath.Join(dataDir, "providers"))
	if err != nil {
		logger.Fatal().Err(err).Msg("loading providers failed")
	}

	agg := aggregator.New(providers, sharedHTTPClient)
	serverAggregator = agg

	app := fiber.New(fiber.Config{
		JSONEncoder: func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
		JSONDecoder: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },

		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	})

	// Instrument all incoming requests: creates a server span, always extracts
	// W3C TraceContext/Baggage from incoming request headers (so the BFF can
	// join an existing trace), and records HTTP metrics.
	// downstreamPropagator() controls whether traceparent/tracestate are also
	// written into the BFF's HTTP response (OTEL_PROPAGATE_DOWNSTREAM, default: true).
	app.Use(otelfiber.Middleware(
		otelfiber.WithPropagators(downstreamPropagator()),
	))

	RegisterRoutes(app, agg, sharedHTTPClient)

	app.Get("/health", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	app.Get("/metrics", func(c fiber.Ctx) error {
		if prometheusRegistry == nil {
			return c.Status(503).SendString("metrics not available")
		}

		// Create a Prometheus HTTP handler using the pre-registered registry.
		handler := promhttp.HandlerFor(prometheusRegistry, promhttp.HandlerOpts{})

		// Create a mock HTTP request and response writer to capture metrics output.
		req, _ := http.NewRequest("GET", "/metrics", nil)
		var buf bytes.Buffer
		handler.ServeHTTP(&mockResponseWriter{Writer: &buf}, req)

		c.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		return c.SendString(buf.String())
	})

	app.Get("/ready", func(c fiber.Ctx) error {
		serverReadyMu.RLock()
		ready := serverReady && serverAggregator != nil
		serverReadyMu.RUnlock()

		if !ready {
			return c.Status(503).SendString("not ready")
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
