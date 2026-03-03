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
	"syscall"
	"time"

	"github.com/gcossani/ssfbff/internal/aggregator"
	otelfiber "github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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

	addr := listenAddr()
	log.Printf("BFF server starting on %s", addr)

	go func() {
		if err := app.Listen(addr); err != nil {
			log.Fatalf("server error: %v", err)
		}
	}()

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
