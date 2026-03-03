//go:build goexperiment.jsonv2

package main

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// initTracing bootstraps OpenTelemetry tracing and registers the global
// TracerProvider and TextMapPropagator. The returned shutdown function must
// be called before the process exits to flush buffered spans.
//
// Everything is configured via standard OTEL_* environment variables:
//
//	OTEL_SDK_DISABLED=true           — disable tracing entirely (no-op)
//	OTEL_TRACES_EXPORTER=none        — disable tracing entirely (no-op)
//	OTEL_DISABLE_TRACING=true        — disable tracing (uses noop exporter, but TracerProvider still exists for per-request override)
//	OTEL_SERVICE_NAME                — service name (default: "ssfbff")
//	OTEL_RESOURCE_ATTRIBUTES         — extra resource key=value pairs
//	OTEL_EXPORTER_OTLP_ENDPOINT      — collector endpoint (default: http://localhost:4318)
//	OTEL_EXPORTER_OTLP_HEADERS       — auth headers, e.g. "Authorization=Bearer <token>"
//	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT — traces-specific endpoint override
//	OTEL_EXPORTER_OTLP_TRACES_HEADERS  — traces-specific header override
//
// When OTEL_DISABLE_TRACING=true, a TracerProvider is still created (to support
// per-request override via x-enable-trace header), but uses a noop exporter.
// See https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/
func initTracing(ctx context.Context) (shutdown func(context.Context) error, err error) {
	noop := func(_ context.Context) error { return nil }

	if os.Getenv("OTEL_SDK_DISABLED") == "true" || os.Getenv("OTEL_TRACES_EXPORTER") == "none" {
		return noop, nil
	}

	// Check if tracing is disabled via OTEL_DISABLE_TRACING.
	// We still create a TracerProvider (for per-request override support),
	// but use a noop exporter.
	disableTracing := os.Getenv("OTEL_DISABLE_TRACING") == "true"

	// resource.WithFromEnv reads OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES.
	// We also supply a default service name in case OTEL_SERVICE_NAME is not set.
	res, resErr := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithAttributes(
			semconv.ServiceNameKey.String(otelServiceName()),
		),
	)
	if resErr != nil {
		// Non-fatal: fall back to the SDK default resource.
		res = resource.Default()
	}

	var tp *sdktrace.TracerProvider
	if disableTracing {
		// When tracing is disabled, create TracerProvider without a batcher.
		// Spans will be created but not exported, supporting per-request override via x-enable-trace header.
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			// No batcher means spans are created but immediately discarded.
			// Note: Per-request override via x-enable-trace header requires a custom sampler.
		)
	} else {
		// otlptracehttp automatically reads OTEL_EXPORTER_OTLP_ENDPOINT,
		// OTEL_EXPORTER_OTLP_HEADERS, and their traces-specific variants,
		// so no manual endpoint configuration is needed here.
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return noop, fmt.Errorf("creating OTLP trace exporter: %w", err)
		}

		tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
			// Default sampler: parentbased_always_on — honours the parent's decision
			// and samples all root spans. Override via OTEL_TRACES_SAMPLER if needed.
		)
	}

	// Register as global so the Fiber otel middleware and otelhttp transport
	// pick up the provider automatically without any direct reference.
	otel.SetTracerProvider(tp)

	// W3C TraceContext propagates trace/span IDs across services.
	// Baggage carries key-value metadata (e.g. tenant ID, user ID).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// otelServiceName returns OTEL_SERVICE_NAME, falling back to "ssfbff".
func otelServiceName() string {
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		return name
	}
	return "ssfbff"
}

// upstreamPropagator returns the propagator used when injecting trace headers
// into outgoing upstream HTTP requests. Set OTEL_PROPAGATE_UPSTREAM=false to
// disable propagation (useful when upstream services don't support W3C tracing).
func upstreamPropagator() propagation.TextMapPropagator {
	if os.Getenv("OTEL_PROPAGATE_UPSTREAM") == "false" {
		return propagation.NewCompositeTextMapPropagator() // noop
	}
	return otel.GetTextMapPropagator()
}

// downstreamPropagator returns the propagator used when writing trace headers
// into BFF HTTP responses. Set OTEL_PROPAGATE_DOWNSTREAM=false to omit
// traceparent/tracestate from responses sent to frontend clients.
func downstreamPropagator() propagation.TextMapPropagator {
	if os.Getenv("OTEL_PROPAGATE_DOWNSTREAM") == "false" {
		return propagation.NewCompositeTextMapPropagator() // noop
	}
	return otel.GetTextMapPropagator()
}
