package agent

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Agent metrics. They are no-ops until the host installs an OpenTelemetry
// MeterProvider (e.g. with a Prometheus or OTLP exporter). Attributes carry
// identifiers and outcomes only, never content.
var (
	meter = observability.Meter("xingding.agent")

	sessionLockWait, _ = meter.Float64Histogram("agent.session_lock.wait",
		metric.WithUnit("s"), metric.WithDescription("Time spent waiting for a session lock, by outcome"))
	runsFinished, _ = meter.Int64Counter("agent.runs",
		metric.WithDescription("Finished agent runs, by status"))
	runDuration, _ = meter.Float64Histogram("agent.run.duration",
		metric.WithUnit("s"), metric.WithDescription("Agent run duration, by status"))
	modelTokens, _ = meter.Int64Counter("agent.model.tokens",
		metric.WithUnit("{token}"), metric.WithDescription("Model tokens by model and kind (input, cached, cache_write, output, reasoning)"))
	modelCost, _ = meter.Float64Counter("agent.model.cost",
		metric.WithUnit("USD"), metric.WithDescription("Estimated model cost by model"))
	circuitOpened, _ = meter.Int64Counter("agent.model.circuit_opened",
		metric.WithDescription("Times a model's circuit breaker opened"))
	quotaRejected, _ = meter.Int64Counter("agent.quota.rejected",
		metric.WithDescription("Model calls refused by a quota, by reason (run_tokens, guard)"))
	cancelledExternally, _ = meter.Int64Counter("agent.run.cancelled_externally",
		metric.WithDescription("Runs stopped because another process cancelled them, by detection (heartbeat, checkpoint, session_lock_lost)"))
	modelRetries, _ = meter.Int64Counter("agent.model.retries",
		metric.WithDescription("Model calls retried after a retryable failure"))
	contextCompactions, _ = meter.Int64Counter("agent.context.compactions",
		metric.WithDescription("Context compactions triggered by the token budget"))
)

// attrs builds measurement attributes. Every key used in this package is a
// fixed identifier/outcome name outside observability's reserved set, so
// the per-call SafeAttrs filtering is unnecessary on this hot path.
func attrs(kv ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributes(kv...)
}

func recordSessionLockWait(ctx context.Context, started time.Time, outcome string) {
	sessionLockWait.Record(ctx, time.Since(started).Seconds(), attrs(attribute.String("outcome", outcome)))
}

func recordRunFinished(ctx context.Context, output *session.RunOutput) {
	ctx = context.WithoutCancel(ctx)
	opt := attrs(attribute.String("status", string(output.Status)), attribute.String("agent_id", output.AgentID))
	runsFinished.Add(ctx, 1, opt)
	if !output.CreatedAt.IsZero() {
		runDuration.Record(ctx, time.Since(output.CreatedAt).Seconds(), opt)
	}
}

func recordModelUsage(ctx context.Context, served models.Model, usage *models.MessageMetrics) {
	if usage == nil || served == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	model := attribute.String("model_id", served.ID())
	provider := attribute.String("provider", served.Provider())
	for _, k := range [...]struct {
		kind string
		n    int
	}{
		{"input", usage.InputTokens}, {"cached", usage.CachedInputTokens}, {"cache_write", usage.CacheWriteTokens},
		{"output", usage.OutputTokens}, {"reasoning", usage.ReasoningTokens},
	} {
		if k.n > 0 {
			modelTokens.Add(ctx, int64(k.n), attrs(model, provider, attribute.String("kind", k.kind)))
		}
	}
	if estimator, ok := served.(models.CostEstimator); ok {
		if cost := estimator.EstimateCost(usage); cost > 0 {
			modelCost.Add(ctx, cost, attrs(model, provider))
		}
	}
}
