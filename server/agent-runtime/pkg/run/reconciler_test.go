package run

import (
	"context"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

func TestReconcilerMarksUnknownAndDrainsOutbox(t *testing.T) {
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}

	journal := NewMemoryExecutionJournal()
	now := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	journal.SetClock(func() time.Time { return now })
	exporter := NewMemoryAuditExporter()

	// An invoking attempt with a requested outbox event, then the worker
	// "crashes" (no outcome).
	if err := journal.BeginAttemptAndAppendRequested(ctx, scope,
		&Attempt{AttemptID: "a1", RunID: "run-1", State: AttemptInvoking, FencingToken: 1, AttemptNumber: 1},
		&AuditRequestedRecord{EventID: "ev-1", AttemptID: "a1", RunID: "run-1", Phase: "tool"},
		&OutboxEvent{EventID: "ev-1", AggregateID: "run-1", Type: "audit.tool.requested"},
	); err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Advance past the reconcile window.
	now = now.Add(2 * time.Minute)

	r := NewReconciler(journal, exporter, scope, "worker-1")
	unknown, exported, err := r.ReconcileOnce(ctx, time.Minute, 10)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if unknown != 1 {
		t.Fatalf("unknown = %d, want 1", unknown)
	}
	if exported != 1 {
		t.Fatalf("exported = %d, want 1", exported)
	}
	if len(exporter.Delivered()) != 1 {
		t.Fatalf("delivered = %d, want 1", len(exporter.Delivered()))
	}
}
