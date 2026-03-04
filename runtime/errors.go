//go:build goexperiment.jsonv2

// Package runtime provides error sanitization utilities to convert internal errors
// into client-safe error messages while protecting implementation details.
package runtime

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
)

// Error codes for programmatic error handling
const (
	ErrorCodeValidationError     = "VALIDATION_ERROR"
	ErrorCodeUpstreamTimeout     = "UPSTREAM_TIMEOUT"
	ErrorCodeUpstreamUnavailable = "UPSTREAM_UNAVAILABLE"
	ErrorCodeUpstreamError       = "UPSTREAM_ERROR"
	ErrorCodeInternalError       = "INTERNAL_ERROR"
	ErrorCodeBadGateway          = "BAD_GATEWAY"
	ErrorCodeInvalidRequest      = "INVALID_REQUEST"
)

// providerEndpointPattern matches provider/endpoint patterns like "user_service/profile"
var providerEndpointPattern = regexp.MustCompile(`\w+_service/\w+:`)

// urlPattern matches URLs in error messages
var urlPattern = regexp.MustCompile(`https?://[^\s]+`)

// SanitizeError converts an internal error into a client-safe error message.
// It removes internal implementation details (provider names, URLs, etc.) and
// maps common error types to user-friendly messages.
//
// The function preserves the original error's context for logging while
// returning a sanitized message suitable for client responses.
func SanitizeError(err error) string {
	if err == nil {
		return "An error occurred processing your request"
	}

	errMsg := err.Error()

	// Remove provider/endpoint names (e.g., "user_service/profile: " → "")
	errMsg = providerEndpointPattern.ReplaceAllString(errMsg, "")

	// Remove URLs
	errMsg = urlPattern.ReplaceAllString(errMsg, "[url]")

	// Check for specific error types and map to user-friendly messages
	if errors.Is(err, context.DeadlineExceeded) {
		return "Request timeout"
	}

	if errors.Is(err, context.Canceled) {
		return "Request cancelled"
	}

	// Check for network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "Request timeout"
		}
		if strings.Contains(errMsg, "connection refused") ||
			strings.Contains(errMsg, "no such host") ||
			strings.Contains(errMsg, "network is unreachable") {
			return "Service temporarily unavailable"
		}
	}

	// Check for DNS errors
	if strings.Contains(errMsg, "no such host") ||
		strings.Contains(errMsg, "server misbehaving") {
		return "Service temporarily unavailable"
	}

	// Check for connection errors
	if strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "broken pipe") {
		return "Service temporarily unavailable"
	}

	// If we've sanitized the message and it's now empty or just whitespace,
	// return a generic message
	errMsg = strings.TrimSpace(errMsg)
	if errMsg == "" || errMsg == ":" {
		return "An error occurred processing your request"
	}

	// Return the sanitized message
	return errMsg
}

// ClassifyError determines the appropriate error code for a given error.
// It analyzes the error to determine if it's a timeout, connection error, etc.
func ClassifyError(err error) string {
	if err == nil {
		return ErrorCodeInternalError
	}

	// Check for timeout errors
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorCodeUpstreamTimeout
	}

	// Check for network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ErrorCodeUpstreamTimeout
		}
		return ErrorCodeUpstreamUnavailable
	}

	errMsg := strings.ToLower(err.Error())

	// Check for connection errors
	if strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "no such host") ||
		strings.Contains(errMsg, "network is unreachable") {
		return ErrorCodeUpstreamUnavailable
	}

	// Check for DNS errors
	if strings.Contains(errMsg, "no such host") ||
		strings.Contains(errMsg, "server misbehaving") {
		return ErrorCodeUpstreamUnavailable
	}

	// Default to internal error for unknown errors
	return ErrorCodeInternalError
}
