package run

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

type controlledClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *controlledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controlledClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type observingManagedStore struct {
	ManagedStore

	mu              sync.Mutex
	events          []string
	nextHeartbeat   error
	heartbeatCalled chan struct{}
	heartbeatOnce   sync.Once
	heartbeatActive atomic.Int32
	releaseCalled   chan struct{}
	releaseOnce     sync.Once
}

type delayedHeartbeatStore struct {
	ManagedStore
	applied chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *delayedHeartbeatStore) Heartbeat(ctx context.Context, scope execution.NamespaceScope, claim *Claim, lease time.Duration) error {
	err := s.ManagedStore.Heartbeat(ctx, scope, claim, lease)
	s.once.Do(func() { close(s.applied) })
	<-s.release
	return err
}

func (s *observingManagedStore) Heartbeat(ctx context.Context, scope execution.NamespaceScope, claim *Claim, lease time.Duration) error {
	s.heartbeatActive.Add(1)
	defer s.heartbeatActive.Add(-1)
	s.heartbeatOnce.Do(func() { close(s.heartbeatCalled) })
	s.mu.Lock()
	err := s.nextHeartbeat
	s.nextHeartbeat = nil
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.ManagedStore.Heartbeat(ctx, scope, claim, lease)
}

func (s *observingManagedStore) CommitManagedRun(ctx context.Context, scope execution.NamespaceScope, claim *Claim, commit ManagedRunCommit) (uint64, error) {
	revision, err := s.ManagedStore.CommitManagedRun(ctx, scope, claim, commit)
	if err == nil {
		s.mu.Lock()
		s.events = append(s.events, "commit:"+string(commit.Run.Status))
		s.mu.Unlock()
	}
	return revision, err
}

func (s *observingManagedStore) Release(ctx context.Context, scope execution.NamespaceScope, claim *Claim) error {
	err := s.ManagedStore.Release(ctx, scope, claim)
	s.mu.Lock()
	s.events = append(s.events, "release")
	s.mu.Unlock()
	s.releaseOnce.Do(func() { close(s.releaseCalled) })
	return err
}

func newObservingStore(clock Clock) (*MemoryDispatcher, *observingManagedStore) {
	base := NewMemoryDispatcher().SetClock(clock)
	return base, &observingManagedStore{
		ManagedStore: base, heartbeatCalled: make(chan struct{}), releaseCalled: make(chan struct{}),
	}
}

func TestManagedRunOwnerUnknownHeartbeatExpiresWithoutExtendingLease(t *testing.T) {
	clock := &controlledClock{now: time.Now().UTC()}
	base, store := newObservingStore(clock.Now)
	unknown := errors.New("heartbeat transport unknown")
	store.nextHeartbeat = unknown
	scope := execution.NamespaceScope{Namespace: "tenant-a"}
	owner, err := AcquireManagedRunOwner(context.Background(), "run-a", ManagedConfig{
		Store: store, Scope: scope, Worker: WorkerIdentity{WorkerID: "worker-a"}, Lease: 100 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond, HeartbeatTimeout: 10 * time.Millisecond, Clock: clock.Now,
	})
	if err != nil {
		t.Fatalf("AcquireManagedRunOwner: %v", err)
	}
	running := &session.RunOutput{RunID: "run-a", Namespace: scope.Namespace, Status: session.RunStatusRunning}
	if _, err := owner.Create(context.Background(), running); err != nil {
		t.Fatalf("Create: %v", err)
	}

	select {
	case <-store.heartbeatCalled:
	case <-time.After(time.Second):
		t.Fatal("heartbeat was not attempted")
	}
	clock.Advance(200 * time.Millisecond)
	claimB, err := base.Claim(context.Background(), scope, running.RunID, WorkerIdentity{WorkerID: "worker-b"}, time.Second)
	if err != nil {
		t.Fatalf("worker B takeover: %v", err)
	}
	snapshot, err := base.GetManagedRun(context.Background(), scope, running.RunID)
	if err != nil {
		t.Fatalf("GetManagedRun: %v", err)
	}
	completed := cloneOutput(snapshot.Run)
	completed.Status = session.RunStatusCompleted
	if _, err := base.CommitManagedRun(context.Background(), scope, claimB, ManagedRunCommit{
		Run: completed, ExpectedRevision: snapshot.Revision, ExpectedStatus: snapshot.Run.Status,
	}); err != nil {
		t.Fatalf("worker B completion: %v", err)
	}

	select {
	case <-owner.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("owner was not cancelled at its last confirmed deadline")
	}
	cause := context.Cause(owner.Context())
	if !errors.Is(cause, ErrOwnershipLost) || !errors.Is(cause, ErrLeaseExpired) || !errors.Is(cause, unknown) {
		t.Fatalf("ownership cause = %v", cause)
	}
	running.Status = session.RunStatusCancelled
	if _, err := owner.Update(context.Background(), running, session.RunStatusRunning); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("stale owner update = %v", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := owner.StopHeartbeat(stopCtx); err != nil {
		t.Fatalf("StopHeartbeat: %v", err)
	}
	if active := store.heartbeatActive.Load(); active != 0 {
		t.Fatalf("heartbeat workers still active: %d", active)
	}
}

func TestManagedRunOwnerUpdatePermitsExpiredCallerDeadline(t *testing.T) {
	base := NewMemoryDispatcher()
	scope := execution.NamespaceScope{Namespace: "tenant-expired-update"}
	owner, err := AcquireManagedRunOwner(context.Background(), "expired-update", ManagedConfig{
		Store: base, Scope: scope, Worker: WorkerIdentity{WorkerID: "worker-a"}, Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("AcquireManagedRunOwner: %v", err)
	}
	running := &session.RunOutput{RunID: "expired-update", Namespace: scope.Namespace, Status: session.RunStatusRunning}
	if _, err := owner.Create(context.Background(), running); err != nil {
		t.Fatalf("Create: %v", err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	completed := cloneOutput(running)
	completed.Status = session.RunStatusCompleted
	if _, err := owner.Update(expired, completed, session.RunStatusRunning); err != nil {
		t.Fatalf("legacy Update with expired caller deadline = %v, want bounded terminal commit", err)
	}
	stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := owner.StopHeartbeat(stopCtx); err != nil {
		t.Fatalf("StopHeartbeat: %v", err)
	}
}

func TestManagedRunOwnerDelayedSuccessDoesNotExtendFromResponseTime(t *testing.T) {
	clock := &controlledClock{now: time.Now().UTC()}
	base := NewMemoryDispatcher().SetClock(clock.Now)
	store := &delayedHeartbeatStore{ManagedStore: base, applied: make(chan struct{}), release: make(chan struct{})}
	owner, err := AcquireManagedRunOwner(context.Background(), "delayed-heartbeat", ManagedConfig{
		Store: store, Scope: execution.NamespaceScope{Namespace: "tenant-a"}, Worker: WorkerIdentity{WorkerID: "worker-a"},
		Lease: 100 * time.Millisecond, HeartbeatInterval: 10 * time.Millisecond, HeartbeatTimeout: 50 * time.Millisecond, Clock: clock.Now,
	})
	if err != nil {
		t.Fatalf("AcquireManagedRunOwner: %v", err)
	}
	select {
	case <-store.applied:
	case <-time.After(time.Second):
		t.Fatal("heartbeat was not applied")
	}
	// The store applied the heartbeat at its call time. Advancing the shared
	// clock past that lease before returning the response must not let the
	// client start a fresh lease window from response time.
	clock.Advance(200 * time.Millisecond)
	close(store.release)
	select {
	case <-owner.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("delayed heartbeat response extended ownership")
	}
	if cause := context.Cause(owner.Context()); !errors.Is(cause, ErrOwnershipLost) || !errors.Is(cause, ErrLeaseExpired) {
		t.Fatalf("ownership cause = %v", cause)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := owner.StopHeartbeat(stopCtx); err != nil {
		t.Fatalf("StopHeartbeat: %v", err)
	}
}

func TestManagedManagerFencesLostExecutorAndPreservesSuccessor(t *testing.T) {
	clock := &controlledClock{now: time.Now().UTC()}
	base, store := newObservingStore(clock.Now)
	scope := execution.NamespaceScope{Namespace: "tenant-a"}
	manager, err := NewManaged(store, time.Second, ManagedConfig{
		Scope: scope, Worker: WorkerIdentity{WorkerID: "worker-a"}, Lease: 100 * time.Millisecond,
		HeartbeatInterval: 10 * time.Millisecond, HeartbeatTimeout: 10 * time.Millisecond, Clock: NowClock,
	})
	if err != nil {
		t.Fatalf("NewManaged: %v", err)
	}
	executorCancelled := make(chan error, 1)
	oldCommit := make(chan error, 1)
	ownerSeen := make(chan *ManagedRunOwner, 1)
	var work atomic.Int32
	executor := func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		owner, ok := ManagedRunOwnerFromContext(ctx)
		if !ok {
			return nil, ErrManagedCommitRequired
		}
		ownerSeen <- owner
		output := &session.RunOutput{RunID: request.RunID, Namespace: scope.Namespace, UserID: "alice", Status: session.RunStatusRunning}
		if _, err := owner.Create(ctx, output); err != nil {
			return output, err
		}
		work.Add(1)
		<-ctx.Done()
		executorCancelled <- context.Cause(ctx)
		output.Status = session.RunStatusCancelled
		_, commitErr := owner.Update(ctx, output, session.RunStatusRunning)
		oldCommit <- commitErr
		return output, context.Cause(ctx)
	}
	started, err := manager.StartManaged(session.RunRequest{RunID: "takeover-run", UserID: "alice"}, executor)
	if err != nil || started.Status != session.RunStatusRunning {
		t.Fatalf("StartManaged = %+v, %v", started, err)
	}

	clock.Advance(200 * time.Millisecond)
	claimB, err := base.Claim(context.Background(), scope, started.RunID, WorkerIdentity{WorkerID: "worker-b"}, time.Second)
	if err != nil {
		t.Fatalf("worker B takeover: %v", err)
	}
	snapshot, err := base.GetManagedRun(context.Background(), scope, started.RunID)
	if err != nil {
		t.Fatalf("worker B snapshot: %v", err)
	}
	completed := cloneOutput(snapshot.Run)
	completed.Status = session.RunStatusCompleted
	completed.Content = "worker-b"
	if _, err := base.CommitManagedRun(context.Background(), scope, claimB, ManagedRunCommit{
		Run: completed, ExpectedRevision: snapshot.Revision, ExpectedStatus: snapshot.Run.Status,
	}); err != nil {
		t.Fatalf("worker B commit: %v", err)
	}

	select {
	case cause := <-executorCancelled:
		if !errors.Is(cause, ErrOwnershipLost) || !errors.Is(cause, ErrStaleFencingToken) {
			t.Fatalf("executor cancellation cause = %v", cause)
		}
	case <-time.After(time.Second):
		t.Fatal("lost executor did not receive cancellation")
	}
	if err := <-oldCommit; !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("old terminal commit = %v", err)
	}
	if got := work.Load(); got != 1 {
		t.Fatalf("work started after cancellation: %d", got)
	}

	ownerA := <-ownerSeen
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := ownerA.StopHeartbeat(stopCtx); err != nil {
		t.Fatalf("wait for manager heartbeat: %v", err)
	}
	if active := store.heartbeatActive.Load(); active != 0 {
		t.Fatalf("manager heartbeat still active: %d", active)
	}
	if err := base.Heartbeat(context.Background(), scope, claimB, time.Second); err != nil {
		t.Fatalf("old owner cleanup changed B lease: %v", err)
	}
	final, err := base.GetManagedRun(context.Background(), scope, started.RunID)
	if err != nil || final.Run.Status != session.RunStatusCompleted || final.Run.Content != "worker-b" {
		t.Fatalf("successor state = %+v, %v", final, err)
	}
}

func TestManagedManagerCommitsTerminalBeforeRelease(t *testing.T) {
	clock := &controlledClock{now: time.Now().UTC()}
	_, store := newObservingStore(clock.Now)
	manager, err := NewManaged(store, time.Second, ManagedConfig{
		Scope: execution.NamespaceScope{Namespace: "tenant-a"}, Worker: WorkerIdentity{WorkerID: "worker-a"},
		Lease: time.Second, HeartbeatInterval: 100 * time.Millisecond, HeartbeatTimeout: 50 * time.Millisecond, Clock: clock.Now,
	})
	if err != nil {
		t.Fatalf("NewManaged: %v", err)
	}
	_, err = manager.StartManaged(session.RunRequest{RunID: "ordered-run"}, func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		owner, _ := ManagedRunOwnerFromContext(ctx)
		output := &session.RunOutput{RunID: request.RunID, Namespace: "tenant-a", Status: session.RunStatusRunning}
		if _, err := owner.Create(ctx, output); err != nil {
			return output, err
		}
		output.Status = session.RunStatusCompleted
		return output, nil
	})
	if err != nil {
		t.Fatalf("StartManaged: %v", err)
	}
	select {
	case <-store.releaseCalled:
	case <-time.After(time.Second):
		t.Fatal("lease was not released")
	}
	store.mu.Lock()
	events := append([]string(nil), store.events...)
	store.mu.Unlock()
	if len(events) < 3 || events[0] != "commit:running" || events[1] != "commit:completed" || events[2] != "release" {
		t.Fatalf("managed lifecycle order = %v", events)
	}
}

func TestManagedManagerCancelRequiresLocalClaim(t *testing.T) {
	store := NewMemoryDispatcher()
	manager, err := NewManaged(store, time.Second, ManagedConfig{
		Scope: execution.NamespaceScope{Namespace: "tenant-a"}, Worker: WorkerIdentity{WorkerID: "worker-a"}, Lease: time.Second,
	})
	if err != nil {
		t.Fatalf("NewManaged: %v", err)
	}
	if _, err := manager.Cancel(context.Background(), db.RunQuery{RunID: "remote", Namespace: "tenant-a"}); !errors.Is(err, ErrManagedLocalClaimRequired) {
		t.Fatalf("remote managed cancel = %v", err)
	}
}

func TestManagedManagerCancelUsesLocalFencedClaim(t *testing.T) {
	store := NewMemoryDispatcher()
	scope := execution.NamespaceScope{Namespace: "tenant-a"}
	manager, err := NewManaged(store, time.Second, ManagedConfig{
		Scope: scope, Worker: WorkerIdentity{WorkerID: "worker-a"}, Lease: time.Second,
	})
	if err != nil {
		t.Fatalf("NewManaged: %v", err)
	}
	exited := make(chan struct{})
	started, err := manager.StartManaged(session.RunRequest{RunID: "cancel-run", UserID: "alice"}, func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		defer close(exited)
		owner, _ := ManagedRunOwnerFromContext(ctx)
		output := &session.RunOutput{RunID: request.RunID, Namespace: scope.Namespace, UserID: request.UserID, Status: session.RunStatusRunning}
		if _, err := owner.Create(ctx, output); err != nil {
			return output, err
		}
		<-ctx.Done()
		output.Status = session.RunStatusCancelled
		return output, context.Cause(ctx)
	})
	if err != nil {
		t.Fatalf("StartManaged: %v", err)
	}
	cancelled, err := manager.Cancel(context.Background(), db.RunQuery{RunID: started.RunID, Namespace: scope.Namespace, UserID: "alice"})
	if err != nil || cancelled.Status != session.RunStatusCancelled {
		t.Fatalf("Cancel = %+v, %v", cancelled, err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("executor did not observe strict cancellation")
	}
	snapshot, err := store.GetManagedRun(context.Background(), scope, started.RunID)
	if err != nil || snapshot.Run.Status != session.RunStatusCancelled {
		t.Fatalf("durable cancellation = %+v, %v", snapshot, err)
	}
}

func TestManagedEntryPointsFailClosedWithoutCapability(t *testing.T) {
	if _, err := NewManaged(nil, time.Second, ManagedConfig{}); !errors.Is(err, ErrManagedStoreRequired) {
		t.Fatalf("NewManaged nil store = %v", err)
	}
	legacy := New(blockingRunStore{}, time.Second)
	if _, err := legacy.StartManaged(session.RunRequest{RunID: "run"}, func(context.Context, session.RunRequest) (*session.RunOutput, error) {
		return nil, nil
	}); !errors.Is(err, ErrManagedStoreRequired) {
		t.Fatalf("legacy StartManaged = %v", err)
	}
}
