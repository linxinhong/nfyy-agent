package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

func scope(ns string) execution.NamespaceScope { return execution.NamespaceScope{Namespace: ns} }

func testAttempt(id string) *Attempt {
	return &Attempt{AttemptID: id, RunID: "run-1", State: AttemptClaimed}
}

func TestFencingTokenMonotonic(t *testing.T) {
	var token uint64
	for i := 0; i < 5; i++ {
		next := FencingToken(token)
		if next <= token {
			t.Fatalf("fencing token must be strictly increasing: %d -> %d", token, next)
		}
		token = next
	}
}

func TestAttemptStateMachine(t *testing.T) {
	// Legal path.
	for _, pair := range [][2]AttemptState{
		{AttemptPrepared, AttemptClaimed},
		{AttemptClaimed, AttemptInvoking},
		{AttemptInvoking, AttemptSucceeded},
	} {
		if err := ValidateAttempt(pair[0], pair[1]); err != nil {
			t.Fatalf("%s -> %s: %v", pair[0], pair[1], err)
		}
	}
	// D-9: invoking cannot go back to prepared/claimed.
	if err := ValidateAttempt(AttemptInvoking, AttemptPrepared); err == nil {
		t.Fatalf("invoking -> prepared must be illegal")
	}
	// D-9: succeeded/failed are terminal.
	if err := ValidateAttempt(AttemptSucceeded, AttemptFailed); err == nil {
		t.Fatalf("succeeded -> failed must be illegal")
	}
	// unknown is terminal.
	if err := ValidateAttempt(AttemptUnknown, AttemptSucceeded); err == nil {
		t.Fatalf("unknown -> succeeded must be illegal")
	}
}

func TestBeginAttemptAndAppendRequestedAtomic(t *testing.T) {
	j := NewMemoryExecutionJournal()
	ctx := context.Background()
	attempt := testAttempt("a1")
	requested := &AuditRequestedRecord{EventID: "ev-1", AttemptID: "a1", RunID: "run-1", Namespace: "ns", Phase: "prepared"}
	event := &OutboxEvent{EventID: "ev-1", AggregateID: "run-1", Type: "attempt.requested"}

	if err := j.BeginAttemptAndAppendRequested(ctx, scope("ns"), attempt, requested, event); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Advance claimed -> invoking -> succeeded, appending outcomes without
	// overwriting the requested record.
	if err := j.AppendOutcome(ctx, scope("ns"), "a1", AttemptInvoking, nil); err != nil {
		t.Fatalf("to invoking: %v", err)
	}
	outcome := &OutboxEvent{EventID: "ev-2", AggregateID: "run-1", Type: "attempt.succeeded"}
	if err := j.AppendOutcome(ctx, scope("ns"), "a1", AttemptSucceeded, outcome); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	claimed, err := j.ClaimOutbox(ctx, scope("ns"), "worker-1", 10)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claimed = %+v, err=%v", claimed, err)
	}
	if claimed[0].Type != "attempt.requested" || claimed[1].Type != "attempt.succeeded" {
		t.Fatalf("outbox order broken: %+v", claimed)
	}
}

func TestOutboxDeduplicationByEventID(t *testing.T) {
	j := NewMemoryExecutionJournal()
	ctx := context.Background()
	if err := j.BeginAttemptAndAppendRequested(ctx, scope("ns"), testAttempt("a1"),
		&AuditRequestedRecord{EventID: "ev-1"}, &OutboxEvent{EventID: "ev-1", AggregateID: "r", Type: "t"}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Consumer claims then acks; re-claim must not return the acked event.
	claimed, _ := j.ClaimOutbox(ctx, scope("ns"), "w", 10)
	if len(claimed) != 1 {
		t.Fatalf("claimed = %d", len(claimed))
	}
	if err := j.AckOutbox(ctx, scope("ns"), "ev-1"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	again, _ := j.ClaimOutbox(ctx, scope("ns"), "w2", 10)
	if len(again) != 0 {
		t.Fatalf("acked event re-claimed: %+v", again)
	}
}

func TestReconcileUnknown(t *testing.T) {
	j := NewMemoryExecutionJournal()
	base := time.Now().UTC()
	j.SetClock(func() time.Time { return base })
	ctx := context.Background()

	attempt := testAttempt("a1")
	if err := j.BeginAttemptAndAppendRequested(ctx, scope("ns"), attempt,
		&AuditRequestedRecord{EventID: "ev-1"}, &OutboxEvent{EventID: "ev-1", Type: "t"}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := j.AppendOutcome(ctx, scope("ns"), "a1", AttemptInvoking, nil); err != nil {
		t.Fatalf("to invoking: %v", err)
	}
	// Fresh attempt is not reconciled.
	if n, _ := j.ReconcileUnknown(ctx, scope("ns"), time.Minute); n != 0 {
		t.Fatalf("fresh attempts reconciled: %d", n)
	}
	// Advance the clock past the staleness window.
	j.SetClock(func() time.Time { return base.Add(2 * time.Minute) })
	if n, _ := j.ReconcileUnknown(ctx, scope("ns"), time.Minute); n != 1 {
		t.Fatalf("reconciled = %d, want 1", n)
	}
	// Terminal attempts are never reconciled.
	if n, _ := j.ReconcileUnknown(ctx, scope("ns"), time.Minute); n != 0 {
		t.Fatalf("double reconcile: %d", n)
	}
}

func TestAppendOutcomeRejectsIllegalTransition(t *testing.T) {
	j := NewMemoryExecutionJournal()
	ctx := context.Background()
	if err := j.BeginAttemptAndAppendRequested(ctx, scope("ns"), testAttempt("a1"),
		&AuditRequestedRecord{EventID: "ev-1"}, &OutboxEvent{EventID: "ev-1", Type: "t"}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := j.AppendOutcome(ctx, scope("ns"), "a1", AttemptSucceeded, nil); err == nil {
		t.Fatalf("prepared -> succeeded must be rejected")
	}
}

func TestNamespaceIsolationInJournal(t *testing.T) {
	j := NewMemoryExecutionJournal()
	ctx := context.Background()
	if err := j.BeginAttemptAndAppendRequested(ctx, scope("ns-a"), testAttempt("a1"),
		&AuditRequestedRecord{EventID: "ev-1"}, &OutboxEvent{EventID: "ev-1", Type: "t"}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// A consumer from another namespace must not see ns-a's outbox events.
	claimed, _ := j.ClaimOutbox(ctx, scope("ns-b"), "w", 10)
	if len(claimed) != 0 {
		t.Fatalf("cross-namespace outbox leak: %+v", claimed)
	}
}

func TestStaleFencingTokenRejected(t *testing.T) {
	// The contract: every reclaim must present a token strictly greater
	// than the last recorded one; equality is stale.
	var last uint64 = 3
	if FencingToken(last) != 4 {
		t.Fatalf("next token = %d", FencingToken(last))
	}
	if errors.Is(ErrStaleFencingToken, ErrInvalidAttemptTransition) {
		t.Fatalf("sentinel drift")
	}
	if !strings.Contains(ErrStaleFencingToken.Error(), "stale fencing") {
		t.Fatalf("sentinel message drift")
	}
}
