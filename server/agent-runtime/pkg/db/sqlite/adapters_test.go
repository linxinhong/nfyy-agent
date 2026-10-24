package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

func newAdapterDB(t *testing.T) *DB {
	t.Helper()
	d, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestSQLitePlanStoreNamespacePartition(t *testing.T) {
	store := NewPlanStore(newAdapterDB(t))
	ctx := context.Background()

	mk := func(ns string) *service.ExecutionPlan {
		return &service.ExecutionPlan{
			ID: "plan-1", Version: "v1", ToolName: "t",
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
	gotA, err := store.Get(ctx, execution.NamespaceScope{Namespace: "ns-a"}, "plan-1")
	if err != nil || gotA.Principal.Namespace != "ns-a" {
		t.Fatalf("ns-a get = %+v, err=%v", gotA, err)
	}
	if _, err := store.Get(ctx, execution.NamespaceScope{Namespace: "ns-c"}, "plan-1"); !errors.Is(err, service.ErrPlanNotFound) {
		t.Fatalf("cross-namespace get err = %v, want not found", err)
	}
}

func TestSQLiteApprovalStoreIdempotentAndConflict(t *testing.T) {
	store := NewApprovalStore(newAdapterDB(t))
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}

	mk := func(decision, nonce string) *service.ApprovalGrant {
		return &service.ApprovalGrant{
			GrantID: "g", PlanID: "plan-1", PlanVersion: "v1", ToolName: "t",
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
	got, err := store.Get(ctx, scope, "plan-1")
	if err != nil || got == nil || got.Decision != "approve" {
		t.Fatalf("get = %+v, err=%v", got, err)
	}
}

func TestSQLiteJournalStoreContract(t *testing.T) {
	store := NewJournalStore(newAdapterDB(t))
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}

	attempt := &run.Attempt{AttemptID: "a1", RunID: "run-1", State: run.AttemptClaimed, FencingToken: 1, AttemptNumber: 1}
	if err := store.BeginAttemptAndAppendRequested(ctx, scope, attempt,
		&run.AuditRequestedRecord{EventID: "ev-1", AttemptID: "a1", RunID: "run-1", Phase: "tool"},
		&run.OutboxEvent{EventID: "ev-1", AggregateID: "run-1", Type: "audit.requested"},
	); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Advance claimed -> invoking -> succeeded.
	if err := store.AppendOutcome(ctx, scope, "a1", run.AttemptInvoking, nil); err != nil {
		t.Fatalf("to invoking: %v", err)
	}
	if err := store.AppendOutcome(ctx, scope, "a1", run.AttemptSucceeded,
		&run.OutboxEvent{EventID: "ev-2", AggregateID: "run-1", Type: "audit.succeeded"},
	); err != nil {
		t.Fatalf("to succeeded: %v", err)
	}
	// Illegal transition is rejected.
	if err := store.AppendOutcome(ctx, scope, "a1", run.AttemptFailed, nil); err == nil {
		t.Fatalf("terminal transition must be rejected")
	}

	// Outbox claim + ack dedup.
	claimed, err := store.ClaimOutbox(ctx, scope, "w", 10)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claimed = %+v, err=%v", claimed, err)
	}
	if err := store.AckOutbox(ctx, scope, "ev-1"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	again, _ := store.ClaimOutbox(ctx, scope, "w", 10)
	if len(again) != 1 || again[0].EventID != "ev-2" {
		t.Fatalf("after ack claimed = %+v", again)
	}
}
