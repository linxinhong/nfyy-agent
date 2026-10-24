package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run/runtest"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

type sqliteManagedFixture struct {
	run.ManagedStore
}

func (f sqliteManagedFixture) CommitManagedRun(ctx context.Context, scope execution.NamespaceScope, claim *run.Claim, commit run.ManagedRunCommit) (uint64, error) {
	copy := *commit.Run
	copy.SessionID = "managed-session"
	copy.AgentID = "managed-agent"
	copy.UserID = "managed-user"
	commit.Run = &copy
	return f.ManagedStore.CommitManagedRun(ctx, scope, claim, commit)
}

func TestSQLiteLeaseDispatcherContract(t *testing.T) {
	runtest.DispatcherContract(t, func(clock run.Clock) run.Dispatcher {
		return NewLeaseDispatcher(newAdapterDB(t)).SetClock(clock)
	})
}

func TestSQLiteManagedStoreContract(t *testing.T) {
	runtest.ManagedStoreContract(t, func(t *testing.T, clock run.Clock) run.ManagedStore {
		store := newAdapterDB(t)
		ctx := context.Background()
		if err := store.CreateSession(ctx, &session.AgentSession{SessionID: "managed-session", AgentID: "managed-agent", UserID: "managed-user", Namespace: "contract"}); err != nil {
			t.Fatalf("create managed parent: %v", err)
		}
		return sqliteManagedFixture{ManagedStore: NewLeaseDispatcher(store).SetClock(clock)}
	})
}

func TestSQLiteManagedStoreTwoConnectionsFenceBeforeSuccessorWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "managed.db")
	firstDB, err := New(path)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	t.Cleanup(func() { _ = firstDB.Close() })
	secondDB, err := New(path)
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() { _ = secondDB.Close() })
	scope := execution.NamespaceScope{Namespace: "managed-two-connections"}
	if err := firstDB.CreateSession(ctx, &session.AgentSession{SessionID: "session-two", AgentID: "agent", UserID: "user", Namespace: scope.Namespace}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	now := time.Date(2026, 9, 6, 18, 0, 0, 123456789, time.UTC)
	clock := func() time.Time { return now }
	first := NewLeaseDispatcher(firstDB).SetClock(clock)
	second := NewLeaseDispatcher(secondDB).SetClock(clock)
	claimA, err := first.Claim(ctx, scope, "run-two", run.WorkerIdentity{WorkerID: "worker-a"}, time.Minute)
	if err != nil {
		t.Fatalf("claim A: %v", err)
	}
	base := &session.RunOutput{
		RunID: claimA.RunID, SessionID: "session-two", AgentID: "agent", UserID: "user",
		Namespace: scope.Namespace, Status: session.RunStatusRunning, Content: "owned-by-a",
		Principal: execution.Principal{Subject: "requester", Namespace: scope.Namespace, Issuer: "test", AuthzEpoch: 7},
		PendingPlans: map[string]session.PendingPlanRef{
			"call-1": {ID: "plan-1", Version: "v1", ApprovalRequired: true},
		},
	}
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
		stale.Principal.Subject = "stale-owner"
		stale.PendingPlans = map[string]session.PendingPlanRef{"call-1": {ID: "stale-plan", Version: "v2"}}
		if _, err := first.CommitManagedRun(ctx, scope, claimA, run.ManagedRunCommit{Run: &stale, ExpectedRevision: revision, ExpectedStatus: session.RunStatusRunning}); !errors.Is(err, run.ErrStaleFencingToken) {
			t.Fatalf("old owner %s commit before B write: %v", status, err)
		}
	}
	beforeB, err := second.GetManagedRun(ctx, scope, claimA.RunID)
	if err != nil {
		t.Fatalf("read before B write: %v", err)
	}
	if beforeB.Revision != 1 || beforeB.Run.Principal.Subject != "requester" || beforeB.Run.Principal.AuthzEpoch != 7 ||
		beforeB.Run.PendingPlans["call-1"].ID != "plan-1" || beforeB.Run.PendingPlans["call-1"].ApprovalRequired != true {
		t.Fatalf("stale write changed frozen resume fields: %+v", beforeB)
	}
	completed := *base
	completed.Status = session.RunStatusCompleted
	completed.Content = "owned-by-b"
	if got, err := second.CommitManagedRun(ctx, scope, claimB, run.ManagedRunCommit{Run: &completed, ExpectedRevision: revision, ExpectedStatus: session.RunStatusRunning}); err != nil || got != 2 {
		t.Fatalf("B commit: revision=%d err=%v", got, err)
	}
	afterB, err := second.GetManagedRun(ctx, scope, claimA.RunID)
	if err != nil || afterB.Revision != 2 || afterB.Run.Content != "owned-by-b" ||
		afterB.Run.Principal.Subject != "requester" || afterB.Run.PendingPlans["call-1"].Version != "v1" {
		t.Fatalf("managed round trip after B write: %+v err=%v", afterB, err)
	}
}

func TestSQLiteManagedStoreRejectsRunIDReuseAcrossKinds(t *testing.T) {
	ctx := context.Background()
	store := newAdapterDB(t)
	scope := execution.NamespaceScope{Namespace: "managed-cross-kind"}
	if err := store.CreateSession(ctx, &session.AgentSession{SessionID: "agent-session", AgentID: "agent", UserID: "user", Namespace: scope.Namespace}); err != nil {
		t.Fatalf("create agent parent: %v", err)
	}
	if err := store.CreateTeamSession(ctx, &session.TeamSession{SessionID: "team-session", TeamID: "team", UserID: "user", Namespace: scope.Namespace}); err != nil {
		t.Fatalf("create team parent: %v", err)
	}
	dispatcher := NewLeaseDispatcher(store)
	claim, err := dispatcher.Claim(ctx, scope, "shared-run-id", run.WorkerIdentity{WorkerID: "worker"}, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	agentRun := &session.RunOutput{
		RunID: claim.RunID, SessionID: "agent-session", AgentID: "agent", UserID: "user",
		Namespace: scope.Namespace, Status: session.RunStatusRunning, Content: "agent-original",
	}
	if revision, err := dispatcher.CommitManagedRun(ctx, scope, claim, run.ManagedRunCommit{Run: agentRun}); err != nil || revision != 1 {
		t.Fatalf("create agent run: revision=%d err=%v", revision, err)
	}
	teamRun := &session.RunOutput{
		RunID: claim.RunID, SessionID: "team-session", TeamID: "team", UserID: "user",
		Namespace: scope.Namespace, Status: session.RunStatusRunning, Content: "team-duplicate",
	}
	if _, err := dispatcher.CommitManagedRun(ctx, scope, claim, run.ManagedRunCommit{Run: teamRun}); !errors.Is(err, dbpkg.ErrConflict) {
		t.Fatalf("reuse run ID across kinds: %v", err)
	}
	snapshot, err := dispatcher.GetManagedRun(ctx, scope, claim.RunID)
	if err != nil || snapshot.Revision != 1 || snapshot.Run.AgentID != "agent" || snapshot.Run.Content != "agent-original" {
		t.Fatalf("original snapshot after rejected cross-kind create: %+v err=%v", snapshot, err)
	}
}

func TestSQLiteLeaseDispatcherGenerationSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "leases.db")
	now := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	scope := execution.NamespaceScope{Namespace: "reopen"}
	lease := time.Minute

	firstDB, err := New(path)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	first := NewLeaseDispatcher(firstDB).SetClock(clock)
	claimA, err := first.Claim(ctx, scope, "run-reopen", run.WorkerIdentity{WorkerID: "worker-a"}, lease)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := first.Release(ctx, scope, claimA); err != nil {
		t.Fatalf("release before reopen: %v", err)
	}
	if err := firstDB.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	secondDB, err := New(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	second := NewLeaseDispatcher(secondDB).SetClock(clock)
	claimB, err := second.Claim(ctx, scope, "run-reopen", run.WorkerIdentity{WorkerID: "worker-b"}, lease)
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
	claimBAgain, err := second.Claim(ctx, scope, "run-reopen", run.WorkerIdentity{WorkerID: "worker-b"}, lease)
	if err != nil || claimBAgain.FencingToken != claimB.FencingToken {
		t.Fatalf("new owner after stale operations = %+v, err=%v", claimBAgain, err)
	}
	if err := secondDB.Close(); err != nil {
		t.Fatalf("close second store: %v", err)
	}

	now = now.Add(lease)
	thirdDB, err := New(path)
	if err != nil {
		t.Fatalf("reopen store after expiry: %v", err)
	}
	t.Cleanup(func() { _ = thirdDB.Close() })
	claimC, err := NewLeaseDispatcher(thirdDB).SetClock(clock).Claim(ctx, scope, "run-reopen", run.WorkerIdentity{WorkerID: "worker-c"}, lease)
	if err != nil {
		t.Fatalf("claim expired lease after second reopen: %v", err)
	}
	if claimC.FencingToken <= claimB.FencingToken {
		t.Fatalf("expired reopen generation = %d, want > %d", claimC.FencingToken, claimB.FencingToken)
	}
}

func TestSQLiteLeaseDispatcherNanosecondExpiryBoundary(t *testing.T) {
	dispatcher := NewLeaseDispatcher(newAdapterDB(t))
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "nanosecond-boundary"}
	base := time.Date(2026, 9, 6, 17, 0, 0, 123456789, time.UTC)
	now := base
	dispatcher.SetClock(func() time.Time { return now })
	lease := time.Second

	beforeExpiry, err := dispatcher.Claim(ctx, scope, "run-before-expiry", run.WorkerIdentity{WorkerID: "worker-a"}, lease)
	if err != nil {
		t.Fatalf("claim before-expiry run: %v", err)
	}
	now = beforeExpiry.LeaseExpires.Add(-100 * time.Microsecond)
	if err := dispatcher.Heartbeat(ctx, scope, beforeExpiry, lease); err != nil {
		t.Fatalf("heartbeat 100us before expiry: %v", err)
	}

	now = base
	atExpiry, err := dispatcher.Claim(ctx, scope, "run-at-expiry", run.WorkerIdentity{WorkerID: "worker-a"}, lease)
	if err != nil {
		t.Fatalf("claim at-expiry run: %v", err)
	}
	now = atExpiry.LeaseExpires
	if err := dispatcher.Heartbeat(ctx, scope, atExpiry, lease); !errors.Is(err, run.ErrStaleFencingToken) {
		t.Fatalf("heartbeat at expiry: %v", err)
	}
	reclaimed, err := dispatcher.Claim(ctx, scope, atExpiry.RunID, run.WorkerIdentity{WorkerID: "worker-b"}, lease)
	if err != nil {
		t.Fatalf("takeover at expiry: %v", err)
	}
	if reclaimed.FencingToken <= atExpiry.FencingToken {
		t.Fatalf("takeover generation = %d, want > %d", reclaimed.FencingToken, atExpiry.FencingToken)
	}
}

func TestSQLiteLeaseDispatcherNamespaceIsolation(t *testing.T) {
	disp := NewLeaseDispatcher(newAdapterDB(t))
	ctx := context.Background()
	lease := time.Minute
	if _, err := disp.Claim(ctx, execution.NamespaceScope{Namespace: "a"}, "run-1", run.WorkerIdentity{WorkerID: "w1"}, lease); err != nil {
		t.Fatalf("claim a: %v", err)
	}
	if _, err := disp.Claim(ctx, execution.NamespaceScope{Namespace: "b"}, "run-1", run.WorkerIdentity{WorkerID: "w1"}, lease); err != nil {
		t.Fatalf("claim b: %v", err)
	}
}
