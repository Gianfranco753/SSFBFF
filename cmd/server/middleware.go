//go:build goexperiment.jsonv2

package main

import (
	"github.com/google/uuid"
	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
)

// requestIDMiddleware generates a unique request ID and adds it to the context and response headers.
func requestIDMiddleware(logger zerolog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		requestID := c.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}
		
		c.Set("X-Request-ID", requestID)
		
		c.Locals("request_id", requestID)
		
		return c.Next()
	}
}

// panicRecoveryMiddleware recovers from panics, logs them with context, and returns 500 errors.
func panicRecoveryMiddleware(logger zerolog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		defer func() {
			if r := recover(); r != nil {
				requestID := c.Get("X-Request-ID", "")
				logger.Error().
					Str("request_id", requestID).
					Str("endpoint", c.Path()).
					Str("method", c.Method()).
					Interface("panic", r).
					Msg("panic recovered")
				
				recordHTTPError(c.Path(), c.Method(), fiber.StatusInternalServerError)
				c.Status(fiber.StatusInternalServerError)
				c.SendString("Internal Server Error")
			}
		}()
		
		return c.Next()
	}
}

// errorHandlerMiddleware handles errors returned by handlers and logs them.
func errorHandlerMiddleware(logger zerolog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		err := c.Next()
		if err != nil {
			requestID := c.Get("X-Request-ID", "")
			statusCode := fiber.StatusInternalServerError
			
			if e, ok := err.(*fiber.Error); ok {
				statusCode = e.Code
			}
			
			if statusCode >= 400 {
				logger.Error().
					Str("request_id", requestID).
					Str("endpoint", c.Path()).
					Str("method", c.Method()).
					Int("status_code", statusCode).
					Err(err).
					Msg("request error")
				
				recordHTTPError(c.Path(), c.Method(), statusCode)
			}
		}
		
		return err
	}
}
