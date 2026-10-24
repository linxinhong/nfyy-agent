package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"
)

// FinishReason is the provider-agnostic reason a model stopped generating.
type FinishReason string

const (
	// FinishReasonUnknown means the provider did not report a reason.
	FinishReasonUnknown FinishReason = ""
	// FinishReasonStop is a natural stop or a stop sequence.
	FinishReasonStop FinishReason = "stop"
	// FinishReasonToolCalls means the model stopped to call tools.
	FinishReasonToolCalls FinishReason = "tool_calls"
	// FinishReasonLength means the output hit the max token limit and is
	// truncated; tool-call arguments may be incomplete JSON.
	FinishReasonLength FinishReason = "length"
	// FinishReasonContentFilter means the provider withheld or cut output.
	FinishReasonContentFilter FinishReason = "content_filter"
	// FinishReasonResourceExhausted is DeepSeek's
	// insufficient_system_resource: inference was interrupted server-side.
	FinishReasonResourceExhausted FinishReason = "resource_exhausted"
)

// NormalizeFinishReason maps provider-specific finish/stop reasons onto the
// shared vocabulary. Unrecognized values are preserved verbatim.
func NormalizeFinishReason(raw string) FinishReason {
	switch raw {
	case "":
		return FinishReasonUnknown
	case "stop", "end_turn", "stop_sequence", "eos":
		return FinishReasonStop
	case "tool_calls", "function_call", "tool_use":
		return FinishReasonToolCalls
	case "length", "max_tokens", "model_context_window_exceeded":
		return FinishReasonLength
	case "content_filter", "refusal", "sensitive":
		return FinishReasonContentFilter
	case "insufficient_system_resource":
		return FinishReasonResourceExhausted
	default:
		return FinishReason(raw)
	}
}

// ErrStreamInterrupted reports that a stream ended (EOF or a broken
// connection) before the provider signaled completion. The partial output
// must not be treated as a finished answer.
var ErrStreamInterrupted = errors.New("models: stream ended before completion")

// ErrResourceExhausted reports a server-side interruption signaled through
// the finish reason (DeepSeek insufficient_system_resource). It is retryable.
var ErrResourceExhausted = errors.New("models: provider interrupted inference (insufficient system resource)")

// ProviderError is a typed, bounded model-provider HTTP failure. Message is
// extracted from the provider's error envelope and truncated; raw response
// bodies are never carried.
type ProviderError struct {
	Provider   string
	StatusCode int
	// Code is the provider error type/code (e.g. "rate_limit_error").
	Code    string
	Message string
	// RetryAfter is the server-advised delay (Retry-After header), or zero.
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string {
	msg := fmt.Sprintf("%s: HTTP %d", e.Provider, e.StatusCode)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Retryable reports whether the status code denotes a transient failure.
func (e *ProviderError) Retryable() bool {
	switch e.StatusCode {
	case 408, 409, 425, 429, 500, 502, 503, 504, 529:
		return true
	default:
		return false
	}
}

// RetryableError lets custom Model implementations classify their own
// errors for the agent retry policy.
type RetryableError interface {
	Retryable() bool
}

// retryAfterError exposes a server-advised retry delay.
type retryAfterError interface {
	RetryAfterDelay() time.Duration
}

// RetryAfterDelay implements the retry-after contract.
func (e *ProviderError) RetryAfterDelay() time.Duration { return e.RetryAfter }

// IsRetryable classifies err for model-call retry. Classification is typed:
// context errors never retry; RetryableError implementations decide for
// themselves; interrupted streams, resource exhaustion, network timeouts,
// and connection resets retry. Everything else does not.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var classified RetryableError
	if errors.As(err, &classified) {
		return classified.Retryable()
	}
	if errors.Is(err, ErrStreamInterrupted) || errors.Is(err, ErrResourceExhausted) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// RetryAfter returns the server-advised retry delay carried by err, if any.
func RetryAfter(err error) time.Duration {
	var ra retryAfterError
	if errors.As(err, &ra) {
		return ra.RetryAfterDelay()
	}
	return 0
}
