package run

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func newDispatchFixture(t *testing.T) (*in_memory.DB, *MemoryDispatcher, *Manager) {
	t.Helper()
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1", UserID: "alice"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	disp := NewMemoryDispatcher()
	manager := New(store, time.Minute).SetDispatcher(disp, "worker-1", "ns", time.Second)
	return store, disp, manager
}

func waitForRun(t *testing.T, store *in_memory.DB, runID string, status session.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := store.GetRun(context.Background(), db.RunQuery{RunID: runID, UserID: "alice"})
		if err == nil && out != nil && out.Status == status {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", runID, status)
}

func TestManagerDispatchesAndReleasesLease(t *testing.T) {
	store, disp, manager := newDispatchFixture(t)

	executor := func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, output); err != nil {
			return nil, err
		}
		output.Status = session.RunStatusCompleted
		output.Content = "done"
		return output, nil
	}

	pending, err := manager.Start(session.RunRequest{UserID: "alice"}, executor)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForRun(t, store, pending.RunID, session.RunStatusCompleted)

	// The lease must be released after completion. The manager persists the
	// terminal status before releasing the lease, so poll for the release.
	var tombstone *Claim
	released := func() bool {
		disp.mu.Lock()
		tombstone = cloneClaim(disp.leases[leaseKey(execution.NamespaceScope{Namespace: "ns"}, pending.RunID)])
		disp.mu.Unlock()
		return tombstone != nil && tombstone.WorkerID == "" && tombstone.FencingToken != 0 && tombstone.LeaseExpires.IsZero()
	}
	for deadline := time.Now().Add(5 * time.Second); !released() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if !released() {
		t.Fatalf("released lease tombstone = %+v, want no active owner with retained generation", tombstone)
	}
}

func TestManagerLeaseHeldDoesNotExecute(t *testing.T) {
	store, disp, manager := newDispatchFixture(t)

	// Another worker holds the lease for the run id up front.
	scope := execution.NamespaceScope{Namespace: "ns"}
	runID := "held-run"
	if _, err := disp.Claim(context.Background(), scope, runID, WorkerIdentity{WorkerID: "other"}, time.Minute); err != nil {
		t.Fatalf("pre-claim: %v", err)
	}

	var executed atomic.Int32
	executor := func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		executed.Add(1)
		output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
		_ = store.AppendRun(ctx, output)
		return output, nil
	}

	if _, err := manager.Start(session.RunRequest{UserID: "alice", RunID: runID}, executor); err == nil {
		t.Fatalf("Start should fail closed when the lease is held by another worker")
	}
	if executed.Load() != 0 {
		t.Fatalf("executor ran %d times despite a held lease", executed.Load())
	}
}

func TestManagerShutdownRejectsNewStarts(t *testing.T) {
	_, _, manager := newDispatchFixture(t)
	manager.Shutdown(context.Background())
	if _, err := manager.Start(session.RunRequest{UserID: "alice"}, func(context.Context, session.RunRequest) (*session.RunOutput, error) {
		return &session.RunOutput{Status: session.RunStatusCompleted}, nil
	}); err == nil {
		t.Fatalf("Start after Shutdown should fail")
	}
}
