package run

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

func TestLedgerRequestedBeforeOutcome(t *testing.T) {
	journal := NewMemoryExecutionJournal()
	ledger := NewTransactionalAuditLedger(journal)
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}

	attempt := &Attempt{AttemptID: "a1", RunID: "run-1", State: AttemptClaimed}
	requested := AuditEvent{EventID: "ev-1", AttemptID: "a1", RunID: "run-1", Namespace: "ns", Phase: "tool", Outcome: AuditOutcomeRequested}
	if err := ledger.Requested(ctx, scope, attempt, requested); err != nil {
		t.Fatalf("requested: %v", err)
	}
	// Advance claimed -> invoking, then append the succeeded outcome.
	if err := journal.AppendOutcome(ctx, scope, "a1", AttemptInvoking, nil); err != nil {
		t.Fatalf("to invoking: %v", err)
	}
	outcome := AuditEvent{EventID: "ev-2", AttemptID: "a1", RunID: "run-1", Namespace: "ns", Phase: "tool", Outcome: AuditOutcomeSucceeded}
	if err := ledger.Outcome(ctx, scope, "a1", AuditOutcomeSucceeded, "ok", outcome); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	// The requested event exists before any outcome, and both events are in
	// the outbox in order.
	claimed, _ := journal.ClaimOutbox(ctx, scope, "w", 10)
	if len(claimed) != 2 || claimed[0].Type != "audit.tool.requested" || claimed[1].Type != "audit.tool.succeeded" {
		t.Fatalf("outbox = %+v", claimed)
	}
}

func TestLedgerUnknownReconciliation(t *testing.T) {
	journal := NewMemoryExecutionJournal()
	base := time.Now().UTC()
	journal.SetClock(func() time.Time { return base })
	ledger := NewTransactionalAuditLedger(journal)
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}

	attempt := &Attempt{AttemptID: "a1", RunID: "r", State: AttemptClaimed}
	if err := ledger.Requested(ctx, scope, attempt, AuditEvent{EventID: "ev-1", AttemptID: "a1", RunID: "r", Phase: "tool"}); err != nil {
		t.Fatalf("requested: %v", err)
	}
	if err := ledger.Outcome(ctx, scope, "a1", "bogus_outcome", "x", AuditEvent{}); err == nil {
		t.Fatalf("unknown outcome must be rejected")
	}
	// Advance the clock: a claimed attempt without an outcome must not be
	// marked unknown (only invoking attempts reconcile).
	journal.SetClock(func() time.Time { return base.Add(5 * time.Minute) })
	n, err := ledger.ReconcileUnknown(ctx, scope, time.Minute)
	if err != nil || n != 0 {
		t.Fatalf("reconcile non-invoking = %d, %v", n, err)
	}
}

func TestLedgerUnknownOutcomeTransition(t *testing.T) {
	journal := NewMemoryExecutionJournal()
	ledger := NewTransactionalAuditLedger(journal)
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}

	attempt := &Attempt{AttemptID: "a1", RunID: "r", State: AttemptClaimed}
	if err := ledger.Requested(ctx, scope, attempt, AuditEvent{EventID: "ev-1", AttemptID: "a1", RunID: "r", Phase: "tool"}); err != nil {
		t.Fatalf("requested: %v", err)
	}
	if err := ledger.Outcome(ctx, scope, "a1", AuditOutcomeFailed, "boom", AuditEvent{EventID: "ev-2", AttemptID: "a1", RunID: "r"}); err != nil {
		t.Fatalf("claimed -> failed outcome: %v", err)
	}
}

func TestExportAndAckAtLeastOnce(t *testing.T) {
	journal := NewMemoryExecutionJournal()
	ledger := NewTransactionalAuditLedger(journal)
	exporter := NewMemoryAuditExporter()
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}

	attempt := &Attempt{AttemptID: "a1", RunID: "r", State: AttemptClaimed}
	if err := ledger.Requested(ctx, scope, attempt, AuditEvent{EventID: "ev-1", AttemptID: "a1", RunID: "r", Phase: "tool"}); err != nil {
		t.Fatalf("requested: %v", err)
	}

	// First drain fails for ev-1; the event stays unacked.
	exporter.SetFailOn(func(AuditEvent) bool { return true })
	exported, failed, err := ExportAndAck(ctx, journal, scope, "worker-1", exporter, 10)
	if err != nil || exported != 0 || failed != 1 {
		t.Fatalf("first drain = %d exported, %d failed, %v", exported, failed, err)
	}
	// Second drain succeeds; event delivered exactly once by stable ID.
	exporter.SetFailOn(nil)
	exported, failed, err = ExportAndAck(ctx, journal, scope, "worker-1", exporter, 10)
	if err != nil || exported != 1 || failed != 0 {
		t.Fatalf("second drain = %d exported, %d failed, %v", exported, failed, err)
	}
	if len(exporter.Delivered()) != 1 {
		t.Fatalf("delivered = %d, want 1 (dedup by event id)", len(exporter.Delivered()))
	}
}

func TestAuditEventsCarryNoSensitiveBodies(t *testing.T) {
	// AuditEvent's typed fields are the only data; a serialized event must
	// never contain prompt/arguments/result bodies.
	event := AuditEvent{
		EventID: "e", RunID: "r", Namespace: "ns", Subject: "s",
		ToolName: "t", Phase: "tool", Outcome: AuditOutcomeRequested,
		ArgumentsHash: "sha", IdempotencyKeyHash: "sha",
	}
	for _, field := range []string{"Prompt", "Secret", "Token", "Body", "Arguments", "Result"} {
		if hasField(event, field) {
			t.Fatalf("AuditEvent must not carry %s", field)
		}
	}
}

func hasField(v any, name string) bool {
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Name == name {
			return true
		}
	}
	return false
}
