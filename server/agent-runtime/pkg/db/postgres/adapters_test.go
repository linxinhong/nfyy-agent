package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

func TestPostgresPlanStoreNamespacePartition(t *testing.T) {
	d := testDB(t)
	store := NewPlanStore(d)
	ctx := context.Background()
	id := uuid.NewString()

	mk := func(ns string) *service.ExecutionPlan {
		return &service.ExecutionPlan{
			ID: id, Version: "v1", ToolName: "t",
			Arguments:        json.RawMessage(`{"a":1}`),
			ArgumentsHash:    "h",
			RegistryRevision: "reg-1",
			Principal:        execution.Principal{Subject: "s", Namespace: ns},
			PreparedAt:       time.Now().UTC(),
			ExpiresAt:        time.Now().UTC().Add(time.Minute),
		}
	}
	if err := store.Create(ctx, mk("ns-a")); err != nil {
		t.Fatalf("create ns-a: %v", err)
	}
	if err := store.Create(ctx, mk("ns-b")); err != nil {
		t.Fatalf("create ns-b: %v", err)
	}
	gotA, err := store.Get(ctx, execution.NamespaceScope{Namespace: "ns-a"}, id)
	if err != nil || gotA.Principal.Namespace != "ns-a" {
		t.Fatalf("ns-a get = %+v, err=%v", gotA, err)
	}
	if _, err := store.Get(ctx, execution.NamespaceScope{Namespace: "ns-c"}, id); !errors.Is(err, service.ErrPlanNotFound) {
		t.Fatalf("cross-namespace get err = %v, want not found", err)
	}
}

func TestPostgresApprovalStoreIdempotentAndConflict(t *testing.T) {
	d := testDB(t)
	store := NewApprovalStore(d)
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}
	planID := uuid.NewString()

	mk := func(decision, nonce string) *service.ApprovalGrant {
		return &service.ApprovalGrant{
			GrantID: uuid.NewString(), PlanID: planID, PlanVersion: "v1", ToolName: "t",
			ArgumentsHash: "h", Requester: "req", Approver: "app", Decision: decision,
			Nonce: nonce, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
		}
	}
	created, err := store.Resolve(ctx, scope, mk("approve", "n1"))
	if err != nil || !created {
		t.Fatalf("first resolve = %v, %v", created, err)
	}
	// Idempotent same decision + nonce.
	created, err = store.Resolve(ctx, scope, mk("approve", "n1"))
	if err != nil || created {
		t.Fatalf("idempotent resolve = %v, %v", created, err)
	}
	// Different decision conflicts.
	if _, err := store.Resolve(ctx, scope, mk("deny", "n2")); !errors.Is(err, service.ErrApprovalConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	got, err := store.Get(ctx, scope, planID)
	if err != nil || got == nil || got.Decision != "approve" {
		t.Fatalf("get = %+v, err=%v", got, err)
	}
}

func TestPostgresJournalStoreContract(t *testing.T) {
	d := testDB(t)
	store := NewJournalStore(d)
	ctx := context.Background()
	// Unique namespace isolates this test from prior runs' outbox events in
	// the shared PostgreSQL test database.
	scope := execution.NamespaceScope{Namespace: uuid.NewString()}
	attemptID := uuid.NewString()
	runID := uuid.NewString()
	ev1 := uuid.NewString()
	ev2 := uuid.NewString()

	attempt := &run.Attempt{AttemptID: attemptID, RunID: runID, State: run.AttemptClaimed, FencingToken: 1, AttemptNumber: 1}
	if err := store.BeginAttemptAndAppendRequested(ctx, scope, attempt,
		&run.AuditRequestedRecord{EventID: ev1, AttemptID: attemptID, RunID: runID, Phase: "tool"},
		&run.OutboxEvent{EventID: ev1, AggregateID: runID, Type: "audit.requested"},
	); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Advance claimed -> invoking -> succeeded.
	if err := store.AppendOutcome(ctx, scope, attemptID, run.AttemptInvoking, nil); err != nil {
		t.Fatalf("to invoking: %v", err)
	}
	if err := store.AppendOutcome(ctx, scope, attemptID, run.AttemptSucceeded,
		&run.OutboxEvent{EventID: ev2, AggregateID: runID, Type: "audit.succeeded"},
	); err != nil {
		t.Fatalf("to succeeded: %v", err)
	}
	// Illegal transition is rejected.
	if err := store.AppendOutcome(ctx, scope, attemptID, run.AttemptFailed, nil); err == nil {
		t.Fatalf("terminal transition must be rejected")
	}

	// Outbox claim + ack dedup.
	claimed, err := store.ClaimOutbox(ctx, scope, "w", 10)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claimed = %+v, err=%v", claimed, err)
	}
	if err := store.AckOutbox(ctx, scope, ev1); err != nil {
		t.Fatalf("ack: %v", err)
	}
	again, _ := store.ClaimOutbox(ctx, scope, "w", 10)
	if len(again) != 1 || again[0].EventID != ev2 {
		t.Fatalf("after ack claimed = %+v", again)
	}
}
