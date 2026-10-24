package agent

import (
	"context"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

type metricsContextKey struct{}

func contextWithMetrics(ctx context.Context, metrics *session.RunMetrics) context.Context {
	return context.WithValue(ctx, metricsContextKey{}, metrics)
}

func metricsFromContext(ctx context.Context) *session.RunMetrics {
	metrics, _ := ctx.Value(metricsContextKey{}).(*session.RunMetrics)
	return metrics
}

func recordModelCall(ctx context.Context) {
	if metrics := metricsFromContext(ctx); metrics != nil {
		metrics.ModelCalls++
	}
}

func addUsage(metrics *session.RunMetrics, usage *models.MessageMetrics) {
	if metrics == nil || usage == nil {
		return
	}
	metrics.InputTokens += usage.InputTokens
	metrics.OutputTokens += usage.OutputTokens
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	metrics.TotalTokens += total
}

func finishMetrics(output *session.RunOutput) {
	if output == nil || output.CreatedAt.IsZero() {
		return
	}
	output.Metrics.DurationMS = time.Since(output.CreatedAt).Milliseconds()
}
