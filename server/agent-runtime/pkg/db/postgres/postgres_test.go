package postgres

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/dbtest"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestStoreContract(t *testing.T) {
	store := testDB(t)
	dbtest.RunStoreContract(t, store)
	dbtest.WorkflowResumeContract(t, store)
}

func TestScopedSessionAndRunIsolation(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	suffix := uuid.NewString()
	create := func(namespace, id string) *session.AgentSession {
		s := &session.AgentSession{SessionID: id, AgentID: "agent", UserID: "same-user", Namespace: namespace}
		if err := store.CreateSessionScoped(ctx, s); err != nil {
			t.Fatalf("create %s: %v", namespace, err)
		}
		return s
	}
	a, b := create("tenant-a-"+suffix, "session-a-"+suffix), create("tenant-b-"+suffix, "session-b-"+suffix)
	// The physical namespace column is authoritative even if a historical payload
	// contains a stale or forged Namespace field.
	if _, err := store.db.ExecContext(ctx, `UPDATE agent_runtime_sessions
		SET payload=jsonb_set(payload, '{namespace}', to_jsonb('forged'::text))
		WHERE namespace=$1 AND session_id=$2 AND kind='agent'`, a.Namespace, a.SessionID); err != nil {
		t.Fatalf("tamper payload namespace: %v", err)
	}
	if got, err := store.ReadSessionScoped(ctx, a.Namespace, a.SessionID); err != nil || got == nil || got.Namespace != a.Namespace {
		t.Fatalf("physical namespace was not authoritative = %+v, %v", got, err)
	}
	legacy := &session.AgentSession{SessionID: "legacy-" + suffix, AgentID: "agent", UserID: "same-user"}
	if err := store.CreateSession(ctx, legacy); err != nil {
		t.Fatalf("create legacy: %v", err)
	}
	if got, err := store.ReadSessionScoped(ctx, a.Namespace, legacy.SessionID); err != nil || got != nil {
		t.Fatalf("tenant read legacy = %+v, %v", got, err)
	}
	if got, err := store.ReadSession(ctx, legacy.SessionID); err != nil || got == nil || got.Namespace != "legacy" {
		t.Fatalf("legacy read = %+v, %v", got, err)
	}
	if err := store.CreateSessionScoped(ctx, &session.AgentSession{SessionID: a.SessionID, AgentID: "agent", UserID: "same-user", Namespace: b.Namespace}); err == nil {
		t.Fatal("same global session ID was accepted")
	}
	if got, err := store.ReadSessionScoped(ctx, a.Namespace, a.SessionID); err != nil || got == nil || got.Namespace != a.Namespace {
		t.Fatalf("collision changed tenant A = %+v, %v", got, err)
	}
	if got, err := store.ReadSessionScoped(ctx, a.Namespace, b.SessionID); err != nil || got != nil {
		t.Fatalf("cross read = %+v, %v", got, err)
	}
	listed, err := store.ListSessionsScoped(ctx, db.SessionQuery{Namespace: a.Namespace})
	if err != nil || len(listed) != 1 || listed[0].SessionID != a.SessionID {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	if _, err := store.UpdateSessionFieldsScoped(ctx, db.SessionFieldsUpdate{SessionID: b.SessionID, Namespace: a.Namespace, ExpectedRevision: 1}); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("cross update = %v", err)
	}
	if err := store.DeleteSessionScoped(ctx, a.Namespace, b.SessionID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("cross delete = %v", err)
	}
	run := &session.RunOutput{RunID: "run-" + suffix, SessionID: a.SessionID, AgentID: "agent", UserID: "same-user", Namespace: a.Namespace, Status: session.RunStatusCompleted}
	if err := store.AppendRun(execution.ContextWithNamespace(ctx, a.Namespace), run); err != nil {
		t.Fatalf("append run: %v", err)
	}
	if got, err := store.GetRunScoped(ctx, db.RunQuery{RunID: run.RunID, Namespace: b.Namespace}); err != nil || got != nil {
		t.Fatalf("cross run = %+v, %v", got, err)
	}
	if _, err := store.ReadSessionScoped(execution.ContextWithNamespace(ctx, a.Namespace), b.Namespace, b.SessionID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("conflicting trusted context = %v", err)
	}
}

func testDB(t testing.TB) *DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		if os.Getenv("POSTGRES_TEST_FAIL_ON_SKIP") != "" {
			t.Fatal("POSTGRES_TEST_DSN is required (POSTGRES_TEST_FAIL_ON_SKIP=1)")
		}
		t.Skip("POSTGRES_TEST_DSN is not set")
	}
	store, err := New(context.Background(), dsn, Options{AutoMigrate: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStoreSessionCASAndAppendRun(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	id := uuid.NewString()
	s := &session.AgentSession{SessionID: id, AgentID: "agent", UserID: "alice"}
	if err := store.CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first, _ := store.ReadSession(ctx, id)
	stale, _ := store.ReadSession(ctx, id)
	first.SessionState = map[string]any{"value": "new"}
	if err := store.UpdateSession(ctx, first); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	stale.SessionState = map[string]any{"value": "stale"}
	if err := store.UpdateSession(ctx, stale); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}
	run := &session.RunOutput{RunID: uuid.NewString(), SessionID: id, AgentID: "agent", UserID: "alice", Status: session.RunStatusCompleted}
	if err := store.AppendRun(ctx, run); err != nil {
		t.Fatalf("AppendRun: %v", err)
	}
	stored, _ := store.ReadSession(ctx, id)
	if stored.Revision != 2 || len(stored.Runs) != 1 || stored.SessionState["value"] != "new" {
		t.Fatalf("stored = %+v", stored)
	}
}
