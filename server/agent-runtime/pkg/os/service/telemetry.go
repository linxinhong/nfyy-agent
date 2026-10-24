package service

import (
	"go.opentelemetry.io/otel/metric"

	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
)

var (
	meter = observability.Meter("xingding.stream")

	streamsActive, _ = meter.Int64UpDownCounter("stream.active",
		metric.WithDescription("Streamed runs currently producing frames on this instance"))
	framesPublished, _ = meter.Int64Counter("stream.frames",
		metric.WithDescription("SSE frames journaled"))
	streamReconnects, _ = meter.Int64Counter("stream.reconnects",
		metric.WithDescription("Stream reattachments with Last-Event-ID, by source (local, shared)"))
	journalWriteErrors, _ = meter.Int64Counter("stream.journal.write_errors",
		metric.WithDescription("Failed batch writes to the shared stream journal (retried on the next flush)"))
)
