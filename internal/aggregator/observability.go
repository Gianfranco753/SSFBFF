package aggregator

import (
	"time"

	"github.com/rs/zerolog"
)

// ObservabilityConfig holds observability settings for the aggregator.
type ObservabilityConfig struct {
	Logger                zerolog.Logger
	RecordUpstreamCall    func(provider, endpoint string, duration time.Duration, status string)
	RecordUpstreamError   func(provider, endpoint, errorType string)
	RecordAggregatorOp    func(status string)
}

// observabilityEnabled returns true if observability is configured.
func (a *Aggregator) observabilityEnabled() bool {
	return a.obsConfig != nil
}

// recordUpstreamCall records metrics for an upstream call.
func (a *Aggregator) recordUpstreamCall(provider, endpoint string, duration time.Duration, status string) {
	if a.observabilityEnabled() && a.obsConfig.RecordUpstreamCall != nil {
		a.obsConfig.RecordUpstreamCall(provider, endpoint, duration, status)
	}
}

// recordUpstreamError records an upstream error.
func (a *Aggregator) recordUpstreamError(provider, endpoint, errorType string) {
	if a.observabilityEnabled() && a.obsConfig.RecordUpstreamError != nil {
		a.obsConfig.RecordUpstreamError(provider, endpoint, errorType)
	}
}

// recordAggregatorOperation records aggregator operation status.
func (a *Aggregator) recordAggregatorOperation(status string) {
	if a.observabilityEnabled() && a.obsConfig.RecordAggregatorOp != nil {
		a.obsConfig.RecordAggregatorOp(status)
	}
}
