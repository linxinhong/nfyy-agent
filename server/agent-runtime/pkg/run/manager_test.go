package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

type blockingRunStore struct{}

func (blockingRunStore) GetRun(ctx context.Context, _ db.RunQuery) (*session.RunOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingRunStore) AppendRun(context.Context, *session.RunOutput) error { return nil }
func (blockingRunStore) UpdateRun(context.Context, *session.RunOutput, session.RunStatus) error {
	return nil
}

func TestManagerStartsWithDirectIDAndCancelsIdempotently(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1", UserID: "alice"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	manager := New(store, time.Minute)
	started := make(chan string, 1)
	executor := func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, output); err != nil {
			return nil, err
		}
		started <- request.RunID
		<-ctx.Done()
		output.Status = session.RunStatusCancelled
		return output, ctx.Err()
	}

	pending, err := manager.Start(session.RunRequest{UserID: "alice"}, executor)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if pending.RunID == "" || pending.Status != session.RunStatusRunning {
		t.Fatalf("started run = %+v", pending)
	}
	if got := <-started; got != pending.RunID {
		t.Fatalf("executor run id = %q, want %q", got, pending.RunID)
	}

	first, err := manager.Cancel(context.Background(), db.RunQuery{RunID: pending.RunID, UserID: "alice"})
	if err != nil || first.RunID != pending.RunID {
		t.Fatalf("first cancel = %+v, %v", first, err)
	}
	second, err := manager.Cancel(context.Background(), db.RunQuery{RunID: pending.RunID, UserID: "alice"})
	if err != nil || second.RunID != pending.RunID {
		t.Fatalf("second cancel = %+v, %v", second, err)
	}
	if _, err := manager.Cancel(context.Background(), db.RunQuery{RunID: pending.RunID, UserID: "bob"}); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("cross-user cancel = %v, want not found", err)
	}
}

func TestManagerCancelWinsDurablyWithRunCAS(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1", UserID: "alice"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	manager := New(store, time.Minute)
	executor := func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, output); err != nil {
			return nil, err
		}
		<-ctx.Done()
		output.Status = session.RunStatusCancelled
		return output, ctx.Err()
	}

	started, err := manager.Start(session.RunRequest{UserID: "alice"}, executor)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := manager.Cancel(context.Background(), db.RunQuery{RunID: started.RunID, UserID: "alice"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	durable, err := store.GetRun(context.Background(), db.RunQuery{RunID: started.RunID, UserID: "alice"})
	if err != nil || durable == nil || durable.Status != session.RunStatusCancelled {
		t.Fatalf("durable run = %+v, %v; want cancelled", durable, err)
	}
}

func TestManagerCancelLosesToCompletedRun(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1", UserID: "alice"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	manager := New(store, time.Minute)
	release := make(chan struct{})
	executor := func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, output); err != nil {
			return nil, err
		}
		output.Status = session.RunStatusCompleted
		if err := store.UpdateRun(ctx, output, session.RunStatusRunning); err != nil {
			return output, err
		}
		<-release
		return output, nil
	}

	started, err := manager.Start(session.RunRequest{UserID: "alice"}, executor)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := manager.Cancel(context.Background(), db.RunQuery{RunID: started.RunID, UserID: "alice"}); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("Cancel completed run = %v, want not cancellable", err)
	}
	close(release)
}

func TestManagerRepairsMissingTerminalPersistence(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1", UserID: "alice"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	manager := New(store, time.Minute)
	executor := func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, output); err != nil {
			return nil, err
		}
		output.Status = session.RunStatusCompleted
		output.Content = "done"
		return output, nil
	}

	started, err := manager.Start(session.RunRequest{UserID: "alice"}, executor)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		durable, getErr := store.GetRun(context.Background(), db.RunQuery{RunID: started.RunID, UserID: "alice"})
		if getErr != nil {
			t.Fatalf("GetRun: %v", getErr)
		}
		if durable != nil && durable.Status == session.RunStatusCompleted && durable.Content == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal state was not repaired: %+v", durable)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagerRejectsMissingExecutorAndDuplicateRunID(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	existing := &session.RunOutput{RunID: "existing", SessionID: "s1", AgentID: "a1", Status: session.RunStatusRunning}
	if err := store.AppendRun(context.Background(), existing); err != nil {
		t.Fatalf("append run: %v", err)
	}
	manager := New(store, time.Minute)
	if _, err := manager.Start(session.RunRequest{}, nil); !errors.Is(err, ErrExecutorRequired) {
		t.Fatalf("missing executor = %v", err)
	}
	called := false
	if _, err := manager.Start(session.RunRequest{RunID: "existing"}, func(context.Context, session.RunRequest) (*session.RunOutput, error) {
		called = true
		return nil, nil
	}); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("duplicate run id = %v", err)
	}
	if called {
		t.Fatal("duplicate run invoked executor")
	}
}

func TestManagerRegistrationTimeoutBoundsStoreReads(t *testing.T) {
	manager := New(blockingRunStore{}, 30*time.Millisecond)
	startedAt := time.Now()
	_, err := manager.Start(session.RunRequest{}, func(context.Context, session.RunRequest) (*session.RunOutput, error) {
		return nil, nil
	})
	if !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("Start = %v, want registration timeout", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("registration timeout took %s", elapsed)
	}
}

func TestManagerCancelBoundsStoreReads(t *testing.T) {
	manager := New(blockingRunStore{}, 30*time.Millisecond)
	startedAt := time.Now()
	_, err := manager.Cancel(context.Background(), db.RunQuery{RunID: "blocked"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Cancel = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("cancel timeout took %s", elapsed)
	}
}

func TestManagerStartContextRetainsValuesAndDetachesCancellation(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	type traceKey struct{}
	parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), traceKey{}, "trace-1"))
	cancelParent()
	manager := New(store, time.Second)
	seen := make(chan struct{}, 1)
	_, err := manager.StartContext(parent, session.RunRequest{Metadata: map[string]string{"project_id": "project-a"}}, func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		metadata, _ := session.RunMetadataFromContext(ctx)
		if ctx.Value(traceKey{}) != "trace-1" || ctx.Err() != nil || metadata["project_id"] != "project-a" {
			return nil, errors.New("background context was not retained")
		}
		out := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", Metadata: request.Metadata, Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, out); err != nil {
			return nil, err
		}
		seen <- struct{}{}
		out.Status = session.RunStatusCompleted
		return out, nil
	})
	if err != nil {
		t.Fatalf("StartContext: %v", err)
	}
	<-seen
}

func TestManagerAdmissionQueuesBeforeStartingExecutorAndShutdownCancelsWaiters(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1", UserID: "alice"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	admission, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 1, PerTenant: 1, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	manager := New(store, time.Second).SetAdmission(admission)
	release := make(chan struct{})
	started := make(chan string, 2)
	executor := func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, output); err != nil {
			return nil, err
		}
		started <- request.RunID
		<-release
		output.Status = session.RunStatusCompleted
		return output, nil
	}
	first, err := manager.Start(session.RunRequest{UserID: "alice"}, executor)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if got := <-started; got != first.RunID {
		t.Fatalf("first executor = %q, want %q", got, first.RunID)
	}

	queued := make(chan error, 1)
	go func() {
		_, startErr := manager.StartContext(context.Background(), session.RunRequest{UserID: "alice"}, executor)
		queued <- startErr
	}()
	waitForAdmissionStats(t, admission, 1, 1)
	select {
	case got := <-started:
		t.Fatalf("queued executor started before permit: %q", got)
	default:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	manager.Shutdown(shutdownCtx)
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued Start after shutdown = %v, want context.Canceled", err)
	}
	waitForAdmissionStats(t, admission, 1, 0)
	close(release)
	waitForAdmissionStats(t, admission, 0, 0)
}

func TestManagerAdmissionHoldsNonCooperativeExecutorUntilExit(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1", UserID: "alice"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	admission, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 1, PerTenant: 1, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	manager := New(store, time.Second).SetAdmission(admission)
	exit := make(chan struct{})
	firstStarted := make(chan string, 1)
	firstExecutor := func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, output); err != nil {
			return nil, err
		}
		firstStarted <- request.RunID
		// Deliberately ignore ctx cancellation: capacity must not be returned
		// until this executor really exits.
		<-exit
		output.Status = session.RunStatusCompleted
		return output, nil
	}
	first, err := manager.Start(session.RunRequest{UserID: "alice"}, firstExecutor)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	<-firstStarted
	if _, err := manager.Cancel(context.Background(), db.RunQuery{RunID: first.RunID, UserID: "alice"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	secondStarted := make(chan struct{}, 1)
	secondResult := make(chan error, 1)
	go func() {
		_, startErr := manager.Start(session.RunRequest{UserID: "alice"}, func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
			output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
			if err := store.AppendRun(ctx, output); err != nil {
				return nil, err
			}
			secondStarted <- struct{}{}
			output.Status = session.RunStatusCompleted
			return output, nil
		})
		secondResult <- startErr
	}()
	waitForAdmissionStats(t, admission, 1, 1)
	select {
	case <-secondStarted:
		t.Fatal("second executor started after cancellation but before first executor exited")
	default:
	}
	close(exit)
	if err := <-secondResult; err != nil {
		t.Fatalf("second Start: %v", err)
	}
	<-secondStarted
	waitForAdmissionStats(t, admission, 0, 0)
}

func TestManagerShutdownDoesNotCloseSharedAdmissionController(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1", UserID: "alice"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	admission, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 2, PerTenant: 1, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	_, held, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold admission: %v", err)
	}
	firstManager := New(store, time.Second).SetAdmission(admission)
	secondManager := New(store, time.Second).SetAdmission(admission)
	firstResult := make(chan error, 1)
	go func() {
		_, startErr := firstManager.StartContext(context.Background(), session.RunRequest{UserID: "alice"}, noOpStoredExecutor(store))
		firstResult <- startErr
	}()
	secondResult := make(chan error, 1)
	go func() {
		_, startErr := secondManager.StartContext(context.Background(), session.RunRequest{UserID: "alice"}, noOpStoredExecutor(store))
		secondResult <- startErr
	}()
	waitForAdmissionStats(t, admission, 1, 2)
	firstManager.Shutdown(context.Background())
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first manager waiter = %v, want context.Canceled", err)
	}
	held.Release()
	if err := <-secondResult; err != nil {
		t.Fatalf("second manager shared waiter = %v", err)
	}
	waitForAdmissionStats(t, admission, 0, 0)
}

func TestManagerAdmissionRejectsCancelledCallerBeforeStartingExecutor(t *testing.T) {
	store := in_memory.New()
	admission, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 1, PerTenant: 1})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	manager := New(store, time.Second).SetAdmission(admission)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executed := false
	if _, err := manager.StartContext(ctx, session.RunRequest{}, func(context.Context, session.RunRequest) (*session.RunOutput, error) {
		executed = true
		return nil, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled StartContext = %v, want context.Canceled", err)
	}
	if executed || admission.Stats().Active != 0 {
		t.Fatalf("cancelled caller executed=%v stats=%+v", executed, admission.Stats())
	}
}

func TestManagerAdmissionDetachedRunOwnsPermitAfterParentReleases(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1", UserID: "alice"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	admission, err := NewAdmissionController(AdmissionConfig{Total: 2, Queue: 1, PerTenant: 2, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	parentCtx, outer, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatalf("outer acquire: %v", err)
	}
	manager := New(store, time.Second).SetAdmission(admission)
	exit := make(chan struct{})
	started := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		_, startErr := manager.StartContext(parentCtx, session.RunRequest{UserID: "alice"}, func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
			output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
			if err := store.AppendRun(ctx, output); err != nil {
				return nil, err
			}
			started <- struct{}{}
			<-exit
			output.Status = session.RunStatusCompleted
			return output, nil
		})
		result <- startErr
	}()
	<-started
	if err := <-result; err != nil {
		t.Fatalf("StartContext: %v", err)
	}
	waitForAdmissionStats(t, admission, 2, 0)
	outer.Release()
	waitForAdmissionStats(t, admission, 1, 0)
	close(exit)
	waitForAdmissionStats(t, admission, 0, 0)
}

func TestManagerAdmissionDetachedRunDoesNotBypassFullParentLimit(t *testing.T) {
	store := in_memory.New()
	admission, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 0, PerTenant: 1})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	parentCtx, outer, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatalf("outer acquire: %v", err)
	}
	defer outer.Release()
	manager := New(store, time.Second).SetAdmission(admission)
	executed := false
	if _, err := manager.StartContext(parentCtx, session.RunRequest{}, func(context.Context, session.RunRequest) (*session.RunOutput, error) {
		executed = true
		return nil, nil
	}); !errors.Is(err, ErrAdmissionQueueFull) {
		t.Fatalf("detached StartContext = %v, want ErrAdmissionQueueFull", err)
	}
	if executed || admission.Stats().Active != 1 {
		t.Fatalf("detached full limit executed=%v stats=%+v", executed, admission.Stats())
	}
}

func noOpStoredExecutor(store *in_memory.DB) Executor {
	return func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		output := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", UserID: "alice", Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, output); err != nil {
			return nil, err
		}
		output.Status = session.RunStatusCompleted
		return output, nil
	}
}

func TestManagerStartContextUsesMetadataFromParentContext(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	parent := session.ContextWithRunMetadata(context.Background(), map[string]string{"project_id": "project-from-context"})
	manager := New(store, time.Second)
	seen := make(chan struct{}, 1)
	_, err := manager.StartContext(parent, session.RunRequest{}, func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
		metadata, _ := session.RunMetadataFromContext(ctx)
		if request.Metadata["project_id"] != "project-from-context" || metadata["project_id"] != "project-from-context" {
			return nil, errors.New("parent metadata was not retained")
		}
		out := &session.RunOutput{RunID: request.RunID, SessionID: "s1", AgentID: "a1", Metadata: request.Metadata, Status: session.RunStatusRunning}
		if err := store.AppendRun(ctx, out); err != nil {
			return nil, err
		}
		seen <- struct{}{}
		out.Status = session.RunStatusCompleted
		return out, nil
	})
	if err != nil {
		t.Fatalf("StartContext: %v", err)
	}
	<-seen
}
