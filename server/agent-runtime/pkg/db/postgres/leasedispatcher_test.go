package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run/runtest"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

type postgresManagedFixture struct {
	run.ManagedStore
}

func (f postgresManagedFixture) CommitManagedRun(ctx context.Context, scope execution.NamespaceScope, claim *run.Claim, commit run.ManagedRunCommit) (uint64, error) {
	copy := *commit.Run
	copy.SessionID = "managed-session"
	copy.AgentID = "managed-agent"
	copy.UserID = "managed-user"
	commit.Run = &copy
	return f.ManagedStore.CommitManagedRun(ctx, scope, claim, commit)
}

// TestPostgresLeaseDispatcherFaultInjection exercises the RC-08 multi-worker
// takeover scenario: worker A claims, is killed (no heartbeat), its lease
// expires, worker B takes over with a strictly greater fencing token, and
// A's stale writes are rejected. The clock is injected so no real sleep is
// needed.
func TestPostgresLeaseDispatcherFaultInjection(t *testing.T) {
	d := testDB(t)
	runtest.DispatcherContract(t, func(clock run.Clock) run.Dispatcher {
		return NewLeaseDispatcher(d).SetClock(clock)
	})
}

func TestPostgresManagedStoreContract(t *testing.T) {
	store := testDB(t)
	runtest.ManagedStoreContract(t, func(t *testing.T, clock run.Clock) run.ManagedStore {
		ctx := context.Background()
		if _, err := store.db.ExecContext(ctx, `DELETE FROM agent_runtime_dispatch_leases WHERE namespace=$1`, "contract"); err != nil {
			t.Fatalf("clear contract leases: %v", err)
		}
		if _, err := store.db.ExecContext(ctx, `DELETE FROM agent_runtime_sessions WHERE namespace=$1`, "contract"); err != nil {
			t.Fatalf("clear contract sessions: %v", err)
		}
		if err := store.CreateSession(ctx, &session.AgentSession{SessionID: "managed-session", AgentID: "managed-agent", UserID: "managed-user", Namespace: "contract"}); err != nil {
			t.Fatalf("create managed parent: %v", err)
		}
		return postgresManagedFixture{ManagedStore: NewLeaseDispatcher(store).SetClock(clock)}
	})
}

func TestPostgresManagedStoreTwoConnectionsFenceBeforeSuccessorWrite(t *testing.T) {
	ctx := context.Background()
	firstDB := testDB(t)
	secondDB := testDB(t)
	scope := execution.NamespaceScope{Namespace: "managed-" + uuid.NewString()}
	sessionID := uuid.NewString()
	if err := firstDB.CreateSession(ctx, &session.AgentSession{SessionID: sessionID, AgentID: "agent", UserID: "user", Namespace: scope.Namespace}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	now := time.Date(2026, 9, 6, 18, 30, 0, 123456789, time.UTC)
	clock := func() time.Time { return now }
	first := NewLeaseDispatcher(firstDB).SetClock(clock)
	second := NewLeaseDispatcher(secondDB).SetClock(clock)
	claimA, err := first.Claim(ctx, scope, "run-"+uuid.NewString(), run.WorkerIdentity{WorkerID: "worker-a"}, time.Minute)
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	base := &session.RunOutput{RunID: claimA.RunID, SessionID: sessionID, AgentID: "agent", UserID: "user", Namespace: scope.Namespace, Status: session.RunStatusRunning}
	revision, err := first.CommitManagedRun(ctx, scope, claimA, run.ManagedRunCommit{Run: base})
	if err != nil || revision != 1 {
		t.Fatalf("initial commit: revision=%d err=%v", revision, err)
	}
	now = now.Add(time.Minute)
	claimB, err := second.Claim(ctx, scope, claimA.RunID, run.WorkerIdentity{WorkerID: "worker-b"}, time.Minute)
	if err != nil {
		t.Fatalf("takeover B: %v", err)
	}
	for _, status := range []session.RunStatus{session.RunStatusRunning, session.RunStatusCompleted} {
		stale := *base
		stale.Status = status
		if _, err := first.CommitManagedRun(ctx, scope, claimA, run.ManagedRunCommit{Run: &stale, ExpectedRevision: revision, ExpectedStatus: session.RunStatusRunning}); !errors.Is(err, run.ErrStaleFencingToken) {
			t.Fatalf("old owner %s commit before B write: %v", status, err)
		}
	}
	completed := *base
	completed.Status = session.RunStatusCompleted
	if got, err := second.CommitManagedRun(ctx, scope, claimB, run.ManagedRunCommit{Run: &completed, ExpectedRevision: revision, ExpectedStatus: session.RunStatusRunning}); err != nil || got != 2 {
		t.Fatalf("B commit: revision=%d err=%v", got, err)
	}
}

func TestPostgresLeaseDispatcherConcurrentSingleWinner(t *testing.T) {
	firstDB := testDB(t)
	secondDB := testDB(t)
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "concurrent-" + uuid.NewString()}
	dispatchers := []*LeaseDispatcher{NewLeaseDispatcher(firstDB), NewLeaseDispatcher(secondDB)}

	lease := time.Minute
	const workers = 8
	start := make(chan struct{})
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		w := run.WorkerIdentity{WorkerID: string(rune('a' + i))}
		dispatcher := dispatchers[i%len(dispatchers)]
		go func() {
			<-start
			_, err := dispatcher.Claim(ctx, scope, "run-race", w, lease)
			results <- err
		}()
	}
	close(start)
	wins := 0
	for i := 0; i < workers; i++ {
		if err := <-results; err == nil {
			wins++
		} else if !errors.Is(err, run.ErrLeaseHeld) {
			t.Fatalf("unexpected claim err: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent claims winners = %d, want exactly 1", wins)
	}
}

func TestPostgresLeaseDispatcherGenerationSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	scope := execution.NamespaceScope{Namespace: "reopen-" + uuid.NewString()}
	runID := "run-reopen"
	lease := time.Minute

	firstDB := testDB(t)
	first := NewLeaseDispatcher(firstDB).SetClock(clock)
	claimA, err := first.Claim(ctx, scope, runID, run.WorkerIdentity{WorkerID: "worker-a"}, lease)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := first.Release(ctx, scope, claimA); err != nil {
		t.Fatalf("release before reopen: %v", err)
	}
	if err := firstDB.Close(); err != nil {
		t.Fatalf("close first connection pool: %v", err)
	}

	secondDB := testDB(t)
	second := NewLeaseDispatcher(secondDB).SetClock(clock)
	claimB, err := second.Claim(ctx, scope, runID, run.WorkerIdentity{WorkerID: "worker-b"}, lease)
	if err != nil {
		t.Fatalf("claim after reopen: %v", err)
	}
	if claimB.FencingToken <= claimA.FencingToken {
		t.Fatalf("reopen generation = %d, want > %d", claimB.FencingToken, claimA.FencingToken)
	}
	if err := second.Heartbeat(ctx, scope, claimA, lease); !errors.Is(err, run.ErrStaleFencingToken) {
		t.Fatalf("old heartbeat after reopen reclaim: %v", err)
	}
	if err := second.Release(ctx, scope, claimA); !errors.Is(err, run.ErrStaleFencingToken) {
		t.Fatalf("old release after reopen reclaim: %v", err)
	}
	claimBAgain, err := second.Claim(ctx, scope, runID, run.WorkerIdentity{WorkerID: "worker-b"}, lease)
	if err != nil || claimBAgain.FencingToken != claimB.FencingToken {
		t.Fatalf("new owner after stale operations = %+v, err=%v", claimBAgain, err)
	}
	if err := secondDB.Close(); err != nil {
		t.Fatalf("close second connection pool: %v", err)
	}

	now = now.Add(lease)
	thirdDB := testDB(t)
	claimC, err := NewLeaseDispatcher(thirdDB).SetClock(clock).Claim(ctx, scope, runID, run.WorkerIdentity{WorkerID: "worker-c"}, lease)
	if err != nil {
		t.Fatalf("claim expired lease after second reopen: %v", err)
	}
	if claimC.FencingToken <= claimB.FencingToken {
		t.Fatalf("expired reopen generation = %d, want > %d", claimC.FencingToken, claimB.FencingToken)
	}
}
