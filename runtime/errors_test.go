//go:build goexperiment.jsonv2

package runtime

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestSanitizeError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil returns generic message",
			err:  nil,
			want: "An error occurred processing your request",
		},
		{
			name: "deadline exceeded",
			err:  context.DeadlineExceeded,
			want: "Request timeout",
		},
		{
			name: "canceled",
			err:  context.Canceled,
			want: "Request cancelled",
		},
		{
			name: "wrapped deadline exceeded",
			err:  errors.Join(context.DeadlineExceeded, errors.New("outer")),
			want: "Request timeout",
		},
		{
			name: "provider endpoint redacted",
			err:  errors.New("user_service/profile: something failed"),
			want: "something failed",
		},
		{
			name: "URL redacted",
			err:  errors.New("failed to fetch https://internal.example.com/secret"),
			want: "failed to fetch [url]",
		},
		{
			name: "connection refused",
			err:  errors.New("connection refused"),
			want: "Service temporarily unavailable",
		},
		{
			name: "no such host",
			err:  errors.New("no such host"),
			want: "Service temporarily unavailable",
		},
		{
			name: "network unreachable (plain error preserved)",
			err:  errors.New("network is unreachable"),
			want: "network is unreachable",
		},
		{
			name: "connection reset",
			err:  errors.New("connection reset by peer"),
			want: "Service temporarily unavailable",
		},
		{
			name: "broken pipe",
			err:  errors.New("write tcp: broken pipe"),
			want: "Service temporarily unavailable",
		},
		{
			name: "server misbehaving",
			err:  errors.New("dns server misbehaving"),
			want: "Service temporarily unavailable",
		},
		{
			name: "sanitized message empty then generic",
			err:  errors.New("user_service/profile: "),
			want: "An error occurred processing your request",
		},
		{
			name: "sanitized message only colon then generic",
			err:  errors.New("auth_service/token: "),
			want: "An error occurred processing your request",
		},
		{
			name: "plain message preserved",
			err:  errors.New("validation failed"),
			want: "validation failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeError(tt.err)
			if got != tt.want {
				t.Errorf("SanitizeError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeError_NetErrorTimeout(t *testing.T) {
	err := &net.OpError{Err: &timeoutErr{timeout: true}}
	got := SanitizeError(err)
	if got != "Request timeout" {
		t.Errorf("SanitizeError(net timeout) = %q, want Request timeout", got)
	}
}

type timeoutErr struct{ timeout bool }

func (e *timeoutErr) Error() string   { return "i/o timeout" }
func (e *timeoutErr) Timeout() bool   { return e.timeout }
func (e *timeoutErr) Temporary() bool { return true }

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil returns internal",
			err:  nil,
			want: ErrorCodeInternalError,
		},
		{
			name: "deadline exceeded",
			err:  context.DeadlineExceeded,
			want: ErrorCodeUpstreamTimeout,
		},
		{
			name: "connection refused",
			err:  errors.New("connection refused"),
			want: ErrorCodeUpstreamUnavailable,
		},
		{
			name: "connection reset",
			err:  errors.New("connection reset by peer"),
			want: ErrorCodeUpstreamUnavailable,
		},
		{
			name: "no such host",
			err:  errors.New("no such host"),
			want: ErrorCodeUpstreamUnavailable,
		},
		{
			name: "network unreachable",
			err:  errors.New("network is unreachable"),
			want: ErrorCodeUpstreamUnavailable,
		},
		{
			name: "server misbehaving",
			err:  errors.New("dns server misbehaving"),
			want: ErrorCodeUpstreamUnavailable,
		},
		{
			name: "unknown error",
			err:  errors.New("something else"),
			want: ErrorCodeInternalError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyError(tt.err)
			if got != tt.want {
				t.Errorf("ClassifyError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyError_NetError(t *testing.T) {
	timeoutErr := &timeoutErr{timeout: true}
	got := ClassifyError(timeoutErr)
	if got != ErrorCodeUpstreamTimeout {
		t.Errorf("ClassifyError(net timeout) = %q, want %q", got, ErrorCodeUpstreamTimeout)
	}

	nonTimeoutNetErr := &netErrNotTimeout{msg: "connection refused"}
	got2 := ClassifyError(nonTimeoutNetErr)
	if got2 != ErrorCodeUpstreamUnavailable {
		t.Errorf("ClassifyError(net non-timeout) = %q, want %q", got2, ErrorCodeUpstreamUnavailable)
	}
}

type netErrNotTimeout struct{ msg string }

func (e *netErrNotTimeout) Error() string   { return e.msg }
func (e *netErrNotTimeout) Timeout() bool   { return false }
func (e *netErrNotTimeout) Temporary() bool { return true }
