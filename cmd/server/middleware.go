//go:build goexperiment.jsonv2

package main

import (
	"context"
	"os"
	"strconv"
	"sync"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
)

// logEntry represents a log entry to be processed asynchronously.
type logEntry struct {
	level zerolog.Level
	event *zerolog.Event
	msg   string
	ctx   context.Context
}

var (
	asyncLoggingEnabled = getEnvBool("ASYNC_LOGGING", false)
	errorLoggingEnabled = getEnvBool("ENABLE_ERROR_LOGGING", true)
	logChan             chan *logEntry
	logWorkerOnce        sync.Once
)

// initAsyncLogging initializes the async logging worker if enabled.
func initAsyncLogging(logger zerolog.Logger) {
	if !asyncLoggingEnabled {
		return
	}

	logWorkerOnce.Do(func() {
		bufferSize := 1000
		if size := os.Getenv("ASYNC_LOGGING_BUFFER_SIZE"); size != "" {
			if parsed, err := strconv.Atoi(size); err == nil && parsed > 0 {
				bufferSize = parsed
			}
		}
		logChan = make(chan *logEntry, bufferSize)

		go func() {
			for entry := range logChan {
				entry.event.Msg(entry.msg)
			}
		}()
	})
}

// logAsync logs an entry asynchronously if async logging is enabled, otherwise synchronously.
// When async logging is enabled and the channel is full, the log entry is dropped to avoid blocking.
func logAsync(level zerolog.Level, event *zerolog.Event, msg string, ctx context.Context) {
	if !errorLoggingEnabled {
		return
	}

	if asyncLoggingEnabled && logChan != nil {
		select {
		case logChan <- &logEntry{level: level, event: event, msg: msg, ctx: ctx}:
			// Successfully queued, return immediately (fire-and-forget)
		default:
			// Channel full, drop log to avoid blocking request path
			// Optionally could log synchronously here, but dropping is safer for high throughput
		}
	} else {
		event.Msg(msg)
	}
}

// traceIDMiddleware extracts trace ID from OpenTelemetry span context and sets it as X-Request-ID header.
// This replaces UUID generation since OTel already provides trace IDs, and otelzerolog injects trace_id/span_id into logs.
func traceIDMiddleware() fiber.Handler {
	useTraceIDAsRequestID := getEnvBool("USE_TRACE_ID_AS_REQUEST_ID", true)
	if !useTraceIDAsRequestID {
		// Middleware is a no-op if disabled
		return func(c fiber.Ctx) error {
			return c.Next()
		}
	}

	// Check if tracing is disabled globally - if so, skip span context extraction
	tracingDisabled := os.Getenv("OTEL_DISABLE_TRACING") == "true" || 
		os.Getenv("OTEL_SDK_DISABLED") == "true" ||
		os.Getenv("OTEL_TRACES_EXPORTER") == "none"
	
	if tracingDisabled {
		// No-op when tracing is disabled to avoid span context extraction overhead
		return func(c fiber.Ctx) error {
			return c.Next()
		}
	}

	return func(c fiber.Ctx) error {
		// Check if X-Request-ID is already set by client
		if requestID := c.Get("X-Request-ID"); requestID != "" {
			return c.Next()
		}

		// Extract trace ID from OpenTelemetry span context
		span := trace.SpanFromContext(c.Context())
		if span.SpanContext().IsValid() {
			traceID := span.SpanContext().TraceID().String()
			c.Set("X-Request-ID", traceID)
		}

		return c.Next()
	}
}

// panicRecoveryMiddleware recovers from panics, logs them with context, and returns 500 errors.
// Note: trace_id and span_id are automatically injected into logs via otelzerolog, so we don't need request_id.
func panicRecoveryMiddleware(logger zerolog.Logger) fiber.Handler {
	initAsyncLogging(logger)
	return func(c fiber.Ctx) error {
		defer func() {
			if r := recover(); r != nil {
				logEvent := logger.Error().
					Str("endpoint", c.Path()).
					Str("method", c.Method()).
					Interface("panic", r)
				
				logAsync(zerolog.ErrorLevel, logEvent, "panic recovered", c.Context())
				
				recordHTTPError(c.Path(), c.Method(), fiber.StatusInternalServerError)
				c.Status(fiber.StatusInternalServerError)
				c.SendString("Internal Server Error")
			}
		}()
		
		return c.Next()
	}
}

// errorHandlerMiddleware handles errors returned by handlers and logs them.
// Note: trace_id and span_id are automatically injected into logs via otelzerolog, so we don't need request_id.
func errorHandlerMiddleware(logger zerolog.Logger) fiber.Handler {
	initAsyncLogging(logger)
	return func(c fiber.Ctx) error {
		err := c.Next()
		if err != nil {
			statusCode := fiber.StatusInternalServerError
			
			if e, ok := err.(*fiber.Error); ok {
				statusCode = e.Code
			}
			
			if statusCode >= 400 {
				logEvent := logger.Error().
					Str("endpoint", c.Path()).
					Str("method", c.Method()).
					Int("status_code", statusCode).
					Err(err)
				
				logAsync(zerolog.ErrorLevel, logEvent, "request error", c.Context())
				
				recordHTTPError(c.Path(), c.Method(), statusCode)
			}
		}
		
		return err
	}
}
