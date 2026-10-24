// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
)

const (
	defaultRetryDelay      = 500 * time.Millisecond
	defaultRetryMultiplier = 2
	// maxRetryDelay caps computed backoff; a provider Retry-After may
	// exceed it (providers bound their own advice).
	maxRetryDelay = 30 * time.Second
)

// callModel invokes the primary model with retries, then falls back to
// configured fallback models. Fallback stops as soon as the run context is
// done, so a cancelled run never fans out to every fallback.
func (a *Agent) callModel(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	ctx, span := observability.Tracer("xingding.agent").Start(ctx, "model.call")
	started := time.Now()
	outcome := "succeeded"
	defer func() {
		span.SetAttributes(observability.SafeAttrs(
			attribute.String("model_id", a.cfg.Model.ID()),
			attribute.String(observability.FieldOutcome, outcome),
			attribute.Int64(observability.FieldDurationMs, time.Since(started).Milliseconds()),
		)...)
		span.End()
	}()

	var lastErr error
	candidates := a.modelCandidates()
	for _, i := range a.candidateOrder(len(candidates)) {
		m := candidates[i]
		resp, err := withRetries(ctx, a, func() (*models.ModelResponse, error) {
			return m.Response(ctx, messages, opts)
		})
		a.breakers.record(i, err)
		if err == nil {
			noteServedModel(ctx, m)
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	outcome = "failed"
	return nil, lastErr
}

// callModelStream is the streaming equivalent of callModel. Retries and
// fallback cover establishing the stream; failures after the stream starts
// are handled by the run loop (see modelTurn).
func (a *Agent) callModelStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	ctx, span := observability.Tracer("xingding.agent").Start(ctx, "model.stream")
	started := time.Now()
	outcome := "succeeded"
	defer func() {
		span.SetAttributes(observability.SafeAttrs(
			attribute.String("model_id", a.cfg.Model.ID()),
			attribute.String(observability.FieldOutcome, outcome),
			attribute.Int64(observability.FieldDurationMs, time.Since(started).Milliseconds()),
		)...)
		span.End()
	}()

	var lastErr error
	candidates := a.modelCandidates()
	for _, i := range a.candidateOrder(len(candidates)) {
		m := candidates[i]
		stream, err := withRetries(ctx, a, func() (<-chan models.StreamEvent, error) {
			return m.ResponseStream(ctx, messages, opts)
		})
		a.breakers.record(i, err)
		if err == nil {
			noteServedModel(ctx, m)
			return stream, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	outcome = "failed"
	return nil, lastErr
}

// onlyPrimary is the candidate order without fallbacks; never mutated.
var onlyPrimary = []int{0}

func (a *Agent) modelCandidates() []models.Model {
	if len(a.cfg.FallbackModels) == 0 {
		return []models.Model{a.cfg.Model}
	}
	candidates := make([]models.Model, 0, 1+len(a.cfg.FallbackModels))
	candidates = append(candidates, a.cfg.Model)
	return append(candidates, a.cfg.FallbackModels...)
}

// candidateOrder returns the candidate indexes whose circuit allows a call.
// When every circuit is open it fails open to all candidates rather than
// rejecting the request outright.
func (a *Agent) candidateOrder(n int) []int {
	if a.breakers == nil && n == 1 {
		return onlyPrimary
	}
	order := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if a.breakers.allow(i) {
			order = append(order, i)
		}
	}
	if len(order) == 0 {
		for i := 0; i < n; i++ {
			order = append(order, i)
		}
	}
	return order
}

// withRetries calls fn until it succeeds, the error is not retryable (see
// models.IsRetryable), or RetryConfig.MaxRetries is exhausted.
func withRetries[T any](ctx context.Context, a *Agent, fn func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		recordModelCall(ctx)
		value, err := fn()
		if err == nil {
			return value, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		if attempt >= a.cfg.RetryConfig.MaxRetries || !isRetryableError(err) {
			return zero, err
		}
		modelRetries.Add(ctx, 1)
		if waitErr := a.waitRetry(ctx, attempt, err); waitErr != nil {
			return zero, waitErr
		}
	}
}

// waitRetry sleeps for the backoff of attempt, honoring the provider's
// Retry-After advice, and returns early with ctx's error on cancellation.
func (a *Agent) waitRetry(ctx context.Context, attempt int, cause error) error {
	timer := time.NewTimer(a.retryDelay(attempt, cause))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryDelay is exponential backoff with jitter in [d/2, d], capped at
// maxRetryDelay, or the server-advised Retry-After when that is longer.
func (a *Agent) retryDelay(attempt int, cause error) time.Duration {
	base := a.cfg.RetryConfig.RetryDelay
	if base <= 0 {
		base = defaultRetryDelay
	}
	multiplier := a.cfg.RetryConfig.RetryMultiplier
	if multiplier <= 0 {
		multiplier = defaultRetryMultiplier
	}
	delay := float64(base) * math.Pow(multiplier, float64(attempt))
	if delay > float64(maxRetryDelay) {
		delay = float64(maxRetryDelay)
	}
	delay = delay/2 + rand.Float64()*delay/2
	if advised := models.RetryAfter(cause); advised > time.Duration(delay) {
		return advised
	}
	return time.Duration(delay)
}

// isRetryableError classifies model errors by type, never by message text:
// see models.IsRetryable. Custom models opt in via models.RetryableError.
func isRetryableError(err error) bool {
	return models.IsRetryable(err)
}
