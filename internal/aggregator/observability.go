//go:build goexperiment.jsonv2

package aggregator

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// LogFunc is a function that logs with trace context from the provided context.
type LogFunc func(ctx context.Context, level zerolog.Level, msg string, fields ...func(*zerolog.Event))

// ObservabilityConfig holds observability settings for the aggregator.
type ObservabilityConfig struct {
	Logger              zerolog.Logger
	LogFunc             LogFunc // Optional: if provided, uses async logging with trace IDs
	RecordUpstreamCall  func(provider, endpoint string, duration time.Duration, status string)
	RecordUpstreamError func(provider, endpoint, errorType string)
	RecordAggregatorOp  func(status string)
}

// observabilityEnabled returns true if observability is configured.
// Uses feature flag to avoid nil check overhead.
func (a *Aggregator) observabilityEnabled() bool {
	return a.hasObservability
}

// recordUpstreamCall records metrics for an upstream call.
// Uses feature flag to avoid nil check overhead.
func (a *Aggregator) recordUpstreamCall(provider, endpoint string, duration time.Duration, status string) {
	if a.hasRecordUpstreamCall {
		a.obsConfig.RecordUpstreamCall(provider, endpoint, duration, status)
	}
}

// recordUpstreamError records an upstream error.
// Uses feature flag to avoid nil check overhead.
func (a *Aggregator) recordUpstreamError(provider, endpoint, errorType string) {
	if a.hasRecordUpstreamError {
		a.obsConfig.RecordUpstreamError(provider, endpoint, errorType)
	}
}

// recordAggregatorOperation records aggregator operation status.
// Uses feature flag to avoid nil check overhead.
func (a *Aggregator) recordAggregatorOperation(status string) {
	if a.hasRecordAggregatorOp {
		a.obsConfig.RecordAggregatorOp(status)
	}
}
