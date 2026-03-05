//go:build goexperiment.jsonv2

package main

import (
	"bytes"
	"context"
	"sync"
	"time"

	otelfiber "github.com/gofiber/contrib/v3/otel"
	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
)

// logEntry represents a log entry to be processed asynchronously.
// We store the logger and a function that builds the event so we can recreate it
// in the worker goroutine with the proper trace context.
type logEntry struct {
	logger    zerolog.Logger
	level     zerolog.Level
	fields    map[string]interface{}
	msg       string
	ctx       context.Context
	buildEvent func(zerolog.Logger) *zerolog.Event
}

var (
	errorLoggingEnabled = getCachedEnableErrorLogging()
	logChan             chan *logEntry
	logWorkerOnce        sync.Once
	logWorkerWg          sync.WaitGroup
	logChanClosed        bool
	logChanMu            sync.Mutex
)

// initAsyncLogging initializes the async logging worker.
// All logging is asynchronous to avoid blocking the request path.
func initAsyncLogging(logger zerolog.Logger) {
	logWorkerOnce.Do(func() {
		bufferSize := getCachedAsyncLoggingBufferSize()
		logChan = make(chan *logEntry, bufferSize)

		logWorkerWg.Add(1)
		go func() {
			defer logWorkerWg.Done()
			for entry := range logChan {
				// For async logging, we manually extract trace IDs from the stored context
				// since we're in a different goroutine and need to preserve the trace context.
				span := trace.SpanFromContext(entry.ctx)
				var event *zerolog.Event
				
				if entry.buildEvent != nil {
					event = entry.buildEvent(entry.logger)
				} else {
					// Fallback: create event based on level
					switch entry.level {
					case zerolog.DebugLevel:
						event = entry.logger.Debug()
					case zerolog.InfoLevel:
						event = entry.logger.Info()
					case zerolog.WarnLevel:
						event = entry.logger.Warn()
					case zerolog.ErrorLevel:
						event = entry.logger.Error()
					case zerolog.FatalLevel:
						event = entry.logger.Fatal()
					case zerolog.PanicLevel:
						event = entry.logger.Panic()
					default:
						event = entry.logger.Info()
					}
					// Add stored fields if any
					for k, v := range entry.fields {
						event = event.Interface(k, v)
					}
				}
				
				// Manually add trace context for async logging
				if span.SpanContext().IsValid() {
					event = event.
						Str("trace_id", span.SpanContext().TraceID().String()).
						Str("span_id", span.SpanContext().SpanID().String())
				}
				
				event.Msg(entry.msg)
			}
		}()
	})
}

// logAsync logs an entry asynchronously. All logging is async to avoid blocking the request path.
// When the channel is full, the log entry is dropped to avoid blocking.
// Since zerolog.Event doesn't expose its fields, we accept a function that builds the event.
// This allows us to recreate the event in the worker goroutine with the proper trace context.
func logAsync(level zerolog.Level, buildEvent func(zerolog.Logger) *zerolog.Event, msg string, ctx context.Context, logger zerolog.Logger) {
	if !errorLoggingEnabled {
		return
	}

	if logChan == nil {
		// Worker not initialized yet, initialize it
		initAsyncLogging(logger)
		// After initialization, logChan should be set, but if it's still nil, fall back to sync
		if logChan == nil {
			// Fallback: log synchronously with trace context
			span := trace.SpanFromContext(ctx)
			event := buildEvent(logger)
			if span.SpanContext().IsValid() {
				event = event.
					Str("trace_id", span.SpanContext().TraceID().String()).
					Str("span_id", span.SpanContext().SpanID().String())
			}
			event.Msg(msg)
			return
		}
	}

	logChanMu.Lock()
	closed := logChanClosed
	logChanMu.Unlock()

	if closed {
		// Channel is closed (during shutdown), log synchronously with trace context
		span := trace.SpanFromContext(ctx)
		event := buildEvent(logger)
		if span.SpanContext().IsValid() {
			event = event.
				Str("trace_id", span.SpanContext().TraceID().String()).
				Str("span_id", span.SpanContext().SpanID().String())
		}
		event.Msg(msg)
		return
	}

	// Store logger and builder function for async processing
	select {
	case logChan <- &logEntry{logger: logger, level: level, fields: nil, msg: msg, ctx: ctx, buildEvent: buildEvent}:
		// Successfully queued, return immediately (fire-and-forget)
	default:
		// Channel full, drop log to avoid blocking request path
		recordAsyncLogsDropped(1)
	}
}

// Helper functions for logging with trace IDs. These wrap logAsync to make it easier to use.

// logInfo logs an info message with trace context from the provided context.
func logInfo(ctx context.Context, logger zerolog.Logger, msg string, fields ...func(*zerolog.Event)) {
	buildEvent := func(l zerolog.Logger) *zerolog.Event {
		event := l.Info()
		for _, f := range fields {
			f(event)
		}
		return event
	}
	logAsync(zerolog.InfoLevel, buildEvent, msg, ctx, logger)
}

// logWarn logs a warning message with trace context from the provided context.
func logWarn(ctx context.Context, logger zerolog.Logger, msg string, fields ...func(*zerolog.Event)) {
	buildEvent := func(l zerolog.Logger) *zerolog.Event {
		event := l.Warn()
		for _, f := range fields {
			f(event)
		}
		return event
	}
	logAsync(zerolog.WarnLevel, buildEvent, msg, ctx, logger)
}

// logError logs an error message with trace context from the provided context.
func logError(ctx context.Context, logger zerolog.Logger, msg string, fields ...func(*zerolog.Event)) {
	buildEvent := func(l zerolog.Logger) *zerolog.Event {
		event := l.Error()
		for _, f := range fields {
			f(event)
		}
		return event
	}
	logAsync(zerolog.ErrorLevel, buildEvent, msg, ctx, logger)
}

// shutdownAsyncLogging gracefully shuts down the async logging worker.
// It closes the log channel, waits for the worker to finish processing remaining entries.
// Returns true if shutdown completed successfully, false if timeout occurred.
func shutdownAsyncLogging(timeout time.Duration) bool {
	if logChan == nil {
		return true
	}

	logChanMu.Lock()
	if logChanClosed {
		logChanMu.Unlock()
		return true
	}
	logChanClosed = true
	close(logChan)
	logChanMu.Unlock()

	// Wait for worker to finish with timeout
	done := make(chan struct{})
	go func() {
		logWorkerWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Worker finished successfully
		return true
	case <-time.After(timeout):
		// Timeout - worker didn't finish in time
		return false
	}
}

// otelWithTraceIDMiddleware combines OpenTelemetry instrumentation with trace ID extraction.
// This reduces middleware overhead by combining two related operations into one.
// When OTEL_SDK_DISABLED=true or OTEL_TRACES_EXPORTER=none, returns a no-op middleware.
func otelWithTraceIDMiddleware() fiber.Handler {
	// If OTel is completely disabled, return no-op
	if getCachedOtelSDKDisabled() || getCachedOtelTracesExporter() == "none" {
		return func(c fiber.Ctx) error {
			return c.Next()
		}
	}

	useTraceIDAsRequestID := getCachedUseTraceIDAsRequestID()
	tracingDisabled := getCachedOtelDisableTracing()

	// Create OTel middleware
	otelMiddleware := otelfiber.Middleware(
		otelfiber.WithPropagators(downstreamPropagator()),
	)

	// If trace ID as request ID is disabled or tracing is disabled, just use OTel middleware
	if !useTraceIDAsRequestID || tracingDisabled {
		return otelMiddleware
	}

	// Combined middleware: OTel instrumentation + trace ID extraction
	return func(c fiber.Ctx) error {
		// Run OTel middleware first (creates span and extracts trace context)
		if err := otelMiddleware(c); err != nil {
			return err
		}

		// Extract trace ID and set as X-Request-ID if not already set
		if requestID := c.Get("X-Request-ID"); requestID == "" {
			span := trace.SpanFromContext(c.Context())
			if span.SpanContext().IsValid() {
				traceID := span.SpanContext().TraceID().String()
				c.Set("X-Request-ID", traceID)
			}
		}

		return c.Next()
	}
}

// getEndpointTemplate returns the route template path for metrics.
// It first checks c.Locals("route_template"), falling back to c.Path() if not set.
// This ensures metrics use static template paths (e.g., "/users/{id}") 
// instead of dynamic resolved paths (e.g., "/users/123").
func getEndpointTemplate(c fiber.Ctx) string {
	if template := c.Locals("route_template"); template != nil {
		if path, ok := template.(string); ok && path != "" {
			return path
		}
	}
	// Fallback for routes not registered via apigen (e.g., /health, /metrics)
	return c.Path()
}

// panicRecoveryMiddleware recovers from panics, logs them with context, and returns 500 errors.
// Note: trace_id and span_id are automatically injected into logs in the async worker, so we don't need request_id.
func panicRecoveryMiddleware(logger zerolog.Logger) fiber.Handler {
	
	// Use sync.Pool for error response buffers to avoid allocations
	errorResponsePool := sync.Pool{
		New: func() interface{} {
			return &bytes.Buffer{}
		},
	}
	
	return func(c fiber.Ctx) error {
		defer func() {
			if r := recover(); r != nil {
				buildEvent := func(l zerolog.Logger) *zerolog.Event {
					return l.Error().
						Str("endpoint", getEndpointTemplate(c)).
						Str("method", c.Method()).
						Interface("panic", r)
				}
				
				logAsync(zerolog.ErrorLevel, buildEvent, "panic recovered", c.Context(), logger)
				
				recordHTTPError(getEndpointTemplate(c), c.Method(), fiber.StatusInternalServerError)
				
				// Use sync.Pool buffer instead of fiber.Map to avoid allocation
				buf := errorResponsePool.Get().(*bytes.Buffer)
				buf.Reset()
				defer errorResponsePool.Put(buf)
				
				buf.WriteString(`{"error":"Internal Server Error","status":500,"code":"INTERNAL_ERROR"}`)
				c.Set("Content-Type", "application/json")
				_ = c.Status(fiber.StatusInternalServerError).Send(buf.Bytes())
			}
		}()
		
		return c.Next()
	}
}

// errorHandlerMiddleware handles errors returned by handlers and logs them.
// Note: trace_id and span_id are automatically injected into logs in the async worker, so we don't need request_id.
func errorHandlerMiddleware(logger zerolog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		err := c.Next()
		if err != nil {
			statusCode := fiber.StatusInternalServerError
			
			if e, ok := err.(*fiber.Error); ok {
				statusCode = e.Code
			}
			
			if statusCode >= 400 {
				buildEvent := func(l zerolog.Logger) *zerolog.Event {
					return l.Error().
						Str("endpoint", getEndpointTemplate(c)).
						Str("method", c.Method()).
						Int("status_code", statusCode).
						Err(err)
				}
				
				logAsync(zerolog.ErrorLevel, buildEvent, "request error", c.Context(), logger)
				
				recordHTTPError(getEndpointTemplate(c), c.Method(), statusCode)
			}
		}
		
		return err
	}
}
