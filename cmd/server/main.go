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
	"context"
	jsonv2 "encoding/json/v2"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"bytes"
	"io"

	"github.com/gcossani/ssfbff/internal/aggregator"
	otelfiber "github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"

	"github.com/prometheus/client_golang/prometheus"
	promhttp "github.com/prometheus/client_golang/prometheus/promhttp"

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
	// Initialize OpenTelemetry first so the instrumented transport and Fiber
	// middleware can register spans under the correct global TracerProvider.
	ctx := context.Background()
	shutdownTracing, err := initTracing(ctx)
	if err != nil {
		log.Fatalf("tracing setup: %v", err)
	}
	defer func() {
		if err := shutdownTracing(ctx); err != nil {
			log.Printf("tracing shutdown: %v", err)
		}
	}()

	// Initialize Prometheus metrics exporter.
	// This creates a meter provider that exports metrics in Prometheus format.
	promExporter, err := promexp.New()
	if err != nil {
		log.Fatalf("prometheus exporter setup: %v", err)
	}
	prometheusExporter = promExporter

	// Register the exporter's collector with a Prometheus registry.
	prometheusRegistry = prometheus.NewRegistry()
	if err := prometheusRegistry.Register(promExporter.Collector); err != nil {
		log.Fatalf("registering prometheus collector: %v", err)
	}

	meterProvider := metric.NewMeterProvider(
		metric.WithReader(promExporter),
	)
	otel.SetMeterProvider(meterProvider)
	defer func() {
		if err := meterProvider.Shutdown(ctx); err != nil {
			log.Printf("meter provider shutdown: %v", err)
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
		log.Fatalf("loading providers: %v", err)
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
	log.Printf("BFF server starting on %s", addr)

	go func() {
		if err := app.Listen(addr); err != nil {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Give the server a moment to start listening, then mark as ready.
	time.Sleep(100 * time.Millisecond)
	serverReadyMu.Lock()
	serverReady = true
	serverReadyMu.Unlock()
	log.Println("server ready")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("received %v, shutting down...", sig)

	if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("server stopped")
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
