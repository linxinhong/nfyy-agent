package run

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
)

var (
	meter = observability.Meter("xingding.run")

	admissionWait, _ = meter.Float64Histogram("run.admission.wait",
		metric.WithUnit("s"), metric.WithDescription("Time to obtain run admission, by layer (local, cluster) and outcome"))
	staleRunsExpired, _ = meter.Int64Counter("run.stale_runs.expired",
		metric.WithDescription("Running runs failed because their owner stopped heartbeating"))
	sessionsDeleted, _ = meter.Int64Counter("run.sessions.deleted",
		metric.WithDescription("Idle sessions deleted by retention"))
)

func admissionOutcome(err error) string {
	switch {
	case err == nil:
		return "admitted"
	case err == ErrAdmissionQueueFull:
		return "queue_full"
	case err == ErrAdmissionTimeout:
		return "timeout"
	default:
		return "cancelled"
	}
}

func recordAdmission(ctx context.Context, layer string, started time.Time, err error) {
	admissionWait.Record(context.WithoutCancel(ctx), time.Since(started).Seconds(), metric.WithAttributes(
		attribute.String("layer", layer), attribute.String("outcome", admissionOutcome(err))))
}
