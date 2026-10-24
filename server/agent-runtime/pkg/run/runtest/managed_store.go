// Package runtest provides reusable contract tests and reference fakes for
// the run package's optional managed-execution capabilities.
package runtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// ManagedStoreFactory constructs an empty store using the supplied subtest and clock.
// Adapter tests can reuse ManagedStoreContract by installing the same clock
// in their lease and fenced-commit implementation.
type ManagedStoreFactory func(t *testing.T, clock run.Clock) run.ManagedStore

// ManagedStoreContract verifies the ownership and atomic-commit invariants
// common to every strict managed store.
func ManagedStoreContract(t *testing.T, factory ManagedStoreFactory) {
	t.Helper()

	t.Run("concurrent dual owner claim has one winner", func(t *testing.T) {
		now := time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)
		store := factory(t, func() time.Time { return now })
		scope := execution.NamespaceScope{Namespace: "contract"}
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, workerID := range []string{"worker-a", "worker-b"} {
			workerID := workerID
			go func() {
				<-start
				_, err := store.Claim(context.Background(), scope, "run-race", run.WorkerIdentity{WorkerID: workerID}, time.Minute)
				results <- err
			}()
		}
		close(start)
		var acquired, held int
		for range 2 {
			switch err := <-results; {
			case err == nil:
				acquired++
			case errors.Is(err, run.ErrLeaseHeld):
				held++
			default:
				t.Fatalf("concurrent claim: %v", err)
			}
		}
		if acquired != 1 || held != 1 {
			t.Fatalf("concurrent claims: acquired=%d held=%d", acquired, held)
		}
	})

	t.Run("dual owner and expiry takeover fence old owner before successor write", func(t *testing.T) {
		now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
		store := factory(t, func() time.Time { return now })
		scope := execution.NamespaceScope{Namespace: "contract"}
		first, err := store.Claim(context.Background(), scope, "run-takeover", run.WorkerIdentity{WorkerID: "worker-a"}, time.Minute)
		if err != nil {
			t.Fatalf("claim first owner: %v", err)
		}
		if _, err := store.Claim(context.Background(), scope, first.RunID, run.WorkerIdentity{WorkerID: "worker-b"}, time.Minute); !errors.Is(err, run.ErrLeaseHeld) {
			t.Fatalf("claim live lease with second owner: %v", err)
		}
		revision, err := store.CommitManagedRun(context.Background(), scope, first, run.ManagedRunCommit{
			Run: &session.RunOutput{RunID: first.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
		})
		if err != nil || revision != 1 {
			t.Fatalf("initial managed commit: revision=%d err=%v", revision, err)
		}

		now = now.Add(time.Minute)
		second, err := store.Claim(context.Background(), scope, first.RunID, run.WorkerIdentity{WorkerID: "worker-b"}, time.Minute)
		if err != nil {
			t.Fatalf("take over expired lease: %v", err)
		}
		if second.FencingToken <= first.FencingToken {
			t.Fatalf("takeover generation = %d, want > %d", second.FencingToken, first.FencingToken)
		}
		// This is deliberately before worker B's first write: takeover itself
		// must fence A, rather than relying on a later run revision change.
		if _, err := store.CommitManagedRun(context.Background(), scope, first, run.ManagedRunCommit{
			Run:              &session.RunOutput{RunID: first.RunID, Namespace: scope.Namespace, Status: session.RunStatusCompleted},
			ExpectedRevision: 1,
			ExpectedStatus:   session.RunStatusRunning,
		}); !errors.Is(err, run.ErrStaleFencingToken) {
			t.Fatalf("old owner commit before successor write: %v", err)
		}
		if err := store.Heartbeat(context.Background(), scope, first, time.Minute); !errors.Is(err, run.ErrStaleFencingToken) {
			t.Fatalf("old owner heartbeat after takeover: %v", err)
		}
		if err := store.Release(context.Background(), scope, first); !errors.Is(err, run.ErrStaleFencingToken) {
			t.Fatalf("old owner release after takeover: %v", err)
		}
		revision, err = store.CommitManagedRun(context.Background(), scope, second, run.ManagedRunCommit{
			Run:              &session.RunOutput{RunID: first.RunID, Namespace: scope.Namespace, Status: session.RunStatusPaused},
			ExpectedRevision: revision,
			ExpectedStatus:   session.RunStatusRunning,
		})
		if err != nil || revision != 2 {
			t.Fatalf("successor first commit: revision=%d err=%v", revision, err)
		}
		if _, err := store.CommitManagedRun(context.Background(), scope, first, run.ManagedRunCommit{
			Run:              &session.RunOutput{RunID: first.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
			ExpectedRevision: revision,
			ExpectedStatus:   session.RunStatusPaused,
		}); !errors.Is(err, run.ErrStaleFencingToken) {
			t.Fatalf("old owner commit after successor write: %v", err)
		}
	})

	t.Run("release reclaim strictly advances generation", func(t *testing.T) {
		now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
		store := factory(t, func() time.Time { return now })
		scope := execution.NamespaceScope{Namespace: "contract"}
		first, err := store.Claim(context.Background(), scope, "run-release", run.WorkerIdentity{WorkerID: "worker-a"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Release(context.Background(), scope, first); err != nil {
			t.Fatalf("release: %v", err)
		}
		second, err := store.Claim(context.Background(), scope, first.RunID, run.WorkerIdentity{WorkerID: "worker-b"}, time.Minute)
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if second.FencingToken <= first.FencingToken {
			t.Fatalf("reclaim generation = %d, want > %d", second.FencingToken, first.FencingToken)
		}
		if _, err := store.CommitManagedRun(context.Background(), scope, first, run.ManagedRunCommit{
			Run: &session.RunOutput{RunID: first.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
		}); !errors.Is(err, run.ErrStaleFencingToken) {
			t.Fatalf("released owner commit after reclaim: %v", err)
		}
	})

	t.Run("commit rejects mismatched scope and run identity", func(t *testing.T) {
		now := time.Date(2026, 9, 6, 10, 30, 0, 0, time.UTC)
		store := factory(t, func() time.Time { return now })
		scope := execution.NamespaceScope{Namespace: "contract"}
		claim, err := store.Claim(context.Background(), scope, "run-bound", run.WorkerIdentity{WorkerID: "worker-a"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CommitManagedRun(context.Background(), execution.NamespaceScope{Namespace: "other"}, claim, run.ManagedRunCommit{
			Run: &session.RunOutput{RunID: claim.RunID, Namespace: "other", Status: session.RunStatusRunning},
		}); !errors.Is(err, run.ErrInvalidClaim) {
			t.Fatalf("commit in mismatched scope: %v", err)
		}
		if _, err := store.CommitManagedRun(context.Background(), scope, claim, run.ManagedRunCommit{
			Run: &session.RunOutput{RunID: "other-run", Namespace: scope.Namespace, Status: session.RunStatusRunning},
		}); !errors.Is(err, run.ErrInvalidClaim) {
			t.Fatalf("commit for mismatched run: %v", err)
		}
	})

	t.Run("expiry rejects commit without takeover", func(t *testing.T) {
		now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
		store := factory(t, func() time.Time { return now })
		scope := execution.NamespaceScope{Namespace: "contract"}
		claim, err := store.Claim(context.Background(), scope, "run-expired", run.WorkerIdentity{WorkerID: "worker-a"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		if _, err := store.CommitManagedRun(context.Background(), scope, claim, run.ManagedRunCommit{
			Run: &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
		}); !errors.Is(err, run.ErrLeaseExpired) {
			t.Fatalf("expired owner commit: %v", err)
		}
		if _, err := store.GetManagedRun(context.Background(), scope, claim.RunID); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("expired commit left run state behind: %v", err)
		}
	})

	t.Run("pause resume checks generation revision and status", func(t *testing.T) {
		now := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
		store := factory(t, func() time.Time { return now })
		scope := execution.NamespaceScope{Namespace: "contract"}
		claim, err := store.Claim(context.Background(), scope, "run-resume", run.WorkerIdentity{WorkerID: "worker-a"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := store.CommitManagedRun(context.Background(), scope, claim, run.ManagedRunCommit{
			Run: &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
		})
		if err != nil {
			t.Fatalf("create running run: %v", err)
		}
		revision, err = store.CommitManagedRun(context.Background(), scope, claim, run.ManagedRunCommit{
			Run:              &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusPaused},
			ExpectedRevision: revision,
			ExpectedStatus:   session.RunStatusRunning,
		})
		if err != nil || revision != 2 {
			t.Fatalf("pause: revision=%d err=%v", revision, err)
		}
		if err := store.Release(context.Background(), scope, claim); err != nil {
			t.Fatalf("release paused owner: %v", err)
		}
		resumer, err := store.Claim(context.Background(), scope, claim.RunID, run.WorkerIdentity{WorkerID: "worker-b"}, time.Minute)
		if err != nil {
			t.Fatalf("claim paused run for resume: %v", err)
		}
		if resumer.FencingToken <= claim.FencingToken {
			t.Fatalf("resume generation = %d, want > %d", resumer.FencingToken, claim.FencingToken)
		}
		if _, err := store.CommitManagedRun(context.Background(), scope, claim, run.ManagedRunCommit{
			Run:              &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
			ExpectedRevision: revision,
			ExpectedStatus:   session.RunStatusPaused,
		}); !errors.Is(err, run.ErrStaleFencingToken) {
			t.Fatalf("released pause owner resumed run: %v", err)
		}
		if _, err := store.CommitManagedRun(context.Background(), scope, resumer, run.ManagedRunCommit{
			Run:              &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
			ExpectedRevision: 1,
			ExpectedStatus:   session.RunStatusPaused,
		}); !errors.Is(err, db.ErrConflict) {
			t.Fatalf("resume with stale revision: %v", err)
		}
		if _, err := store.CommitManagedRun(context.Background(), scope, resumer, run.ManagedRunCommit{
			Run:              &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusCompleted},
			ExpectedRevision: revision,
			ExpectedStatus:   session.RunStatusRunning,
		}); !errors.Is(err, db.ErrConflict) {
			t.Fatalf("resume with wrong expected status: %v", err)
		}
		if _, err := store.CommitManagedRun(context.Background(), scope, resumer, run.ManagedRunCommit{
			Run:              &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusCompleted},
			ExpectedRevision: revision,
			ExpectedStatus:   session.RunStatusPaused,
		}); !errors.Is(err, db.ErrInvalidTransition) {
			t.Fatalf("resume with illegal target transition: %v", err)
		}
		revision, err = store.CommitManagedRun(context.Background(), scope, resumer, run.ManagedRunCommit{
			Run:              &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
			ExpectedRevision: revision,
			ExpectedStatus:   session.RunStatusPaused,
		})
		if err != nil || revision != 3 {
			t.Fatalf("resume: revision=%d err=%v", revision, err)
		}
		snapshot, err := store.GetManagedRun(context.Background(), scope, claim.RunID)
		if err != nil || snapshot.Revision != revision || snapshot.Run.Status != session.RunStatusRunning {
			t.Fatalf("managed snapshot: %+v err=%v", snapshot, err)
		}
	})
}

type leaseState struct {
	claim          run.Claim
	nextGeneration uint64
}

type storedRun struct {
	output   *session.RunOutput
	revision uint64
}

// FakeManagedStore is the in-process reference implementation of
// run.ManagedStore. Lease state and run state share one mutex, demonstrating
// the required atomic domain without pretending to be a production adapter.
type FakeManagedStore struct {
	mu                 sync.Mutex
	clock              run.Clock
	leases             map[string]*leaseState
	runs               map[string]storedRun
	nextHeartbeatError error
}

var _ run.ManagedStore = (*FakeManagedStore)(nil)

// NewFakeManagedStore constructs an empty reference store.
func NewFakeManagedStore(clock run.Clock) *FakeManagedStore {
	if clock == nil {
		clock = run.NowClock
	}
	return &FakeManagedStore{
		clock:  clock,
		leases: make(map[string]*leaseState),
		runs:   make(map[string]storedRun),
	}
}

// FailNextHeartbeat makes the next heartbeat fail before changing the
// authoritative lease, modelling an indeterminate transport/storage result.
func (s *FakeManagedStore) FailNextHeartbeat(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextHeartbeatError = err
}

func managedKey(namespace, runID string) string { return namespace + "\x00" + runID }

// Claim implements run.Dispatcher.
func (s *FakeManagedStore) Claim(_ context.Context, scope execution.NamespaceScope, runID string, worker run.WorkerIdentity, lease time.Duration) (*run.Claim, error) {
	if !scope.HasNamespace() || runID == "" || worker.WorkerID == "" || lease <= 0 {
		return nil, run.ErrInvalidClaim
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	key := managedKey(scope.Namespace, runID)
	state := s.leases[key]
	if state == nil {
		state = &leaseState{}
		s.leases[key] = state
	}
	if state.claim.FencingToken != 0 && now.Before(state.claim.LeaseExpires) {
		if state.claim.WorkerID != worker.WorkerID {
			return nil, run.ErrLeaseHeld
		}
		state.claim.LeaseExpires = now.Add(lease)
		return cloneClaim(&state.claim), nil
	}
	state.nextGeneration++
	state.claim = run.Claim{
		RunID:        runID,
		Namespace:    scope.Namespace,
		WorkerID:     worker.WorkerID,
		FencingToken: state.nextGeneration,
		LeaseExpires: now.Add(lease),
	}
	return cloneClaim(&state.claim), nil
}

// Heartbeat implements run.Dispatcher.
func (s *FakeManagedStore) Heartbeat(_ context.Context, scope execution.NamespaceScope, claim *run.Claim, lease time.Duration) error {
	if lease <= 0 {
		return run.ErrInvalidClaim
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextHeartbeatError != nil {
		err := s.nextHeartbeatError
		s.nextHeartbeatError = nil
		return err
	}
	state, err := s.authorizeLocked(scope, claim)
	if err != nil {
		return err
	}
	state.claim.LeaseExpires = s.clock().Add(lease)
	return nil
}

// Release implements run.Dispatcher. Generation history remains after release
// so recreating ownership cannot reuse an old fencing value.
func (s *FakeManagedStore) Release(_ context.Context, scope execution.NamespaceScope, claim *run.Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.authorizeLocked(scope, claim)
	if err != nil {
		return err
	}
	state.claim = run.Claim{}
	return nil
}

// GetManagedRun implements run.FencedRunStore.
func (s *FakeManagedStore) GetManagedRun(_ context.Context, scope execution.NamespaceScope, runID string) (*run.ManagedRunSnapshot, error) {
	if !scope.HasNamespace() || runID == "" {
		return nil, run.ErrInvalidClaim
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.runs[managedKey(scope.Namespace, runID)]
	if !ok {
		return nil, db.ErrNotFound
	}
	output, err := cloneOutput(stored.output)
	if err != nil {
		return nil, err
	}
	return &run.ManagedRunSnapshot{Run: output, Revision: stored.revision}, nil
}

// CommitManagedRun implements run.FencedRunStore.
func (s *FakeManagedStore) CommitManagedRun(_ context.Context, scope execution.NamespaceScope, claim *run.Claim, commit run.ManagedRunCommit) (uint64, error) {
	if commit.Run == nil || commit.Run.RunID == "" || commit.Run.Namespace != scope.Namespace {
		return 0, run.ErrInvalidClaim
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.authorizeLocked(scope, claim); err != nil {
		return 0, err
	}
	if claim.RunID != commit.Run.RunID {
		return 0, run.ErrInvalidClaim
	}
	key := managedKey(scope.Namespace, commit.Run.RunID)
	stored, exists := s.runs[key]
	if commit.ExpectedRevision == 0 && commit.ExpectedStatus == "" {
		if exists {
			return 0, db.ErrConflict
		}
		output, err := cloneOutput(commit.Run)
		if err != nil {
			return 0, err
		}
		stored = storedRun{output: output, revision: 1}
		s.runs[key] = stored
		return stored.revision, nil
	}
	if !exists {
		return 0, db.ErrNotFound
	}
	if commit.ExpectedRevision == 0 || stored.revision != commit.ExpectedRevision || stored.output.Status != commit.ExpectedStatus {
		return 0, db.ErrConflict
	}
	if !commit.ExpectedStatus.CanTransitionTo(commit.Run.Status) {
		return 0, db.ErrInvalidTransition
	}
	output, err := cloneOutput(commit.Run)
	if err != nil {
		return 0, err
	}
	stored.output = output
	stored.revision++
	s.runs[key] = stored
	return stored.revision, nil
}

// GetRun implements db.RunStore for compatibility with legacy callers.
func (s *FakeManagedStore) GetRun(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	snapshot, err := s.GetManagedRun(ctx, execution.NamespaceScope{Namespace: query.Namespace}, query.RunID)
	if err != nil {
		return nil, err
	}
	if query.UserID != "" && snapshot.Run.UserID != query.UserID {
		return nil, db.ErrNotFound
	}
	return snapshot.Run, nil
}

// AppendRun implements the legacy db.RunStore path. Strict managed callers
// use CommitManagedRun instead.
func (s *FakeManagedStore) AppendRun(_ context.Context, output *session.RunOutput) error {
	if output == nil || output.RunID == "" || !execution.ValidNamespace(output.Namespace) {
		return fmt.Errorf("invalid run")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := managedKey(output.Namespace, output.RunID)
	if _, exists := s.runs[key]; exists {
		return db.ErrConflict
	}
	clone, err := cloneOutput(output)
	if err != nil {
		return err
	}
	s.runs[key] = storedRun{output: clone, revision: 1}
	return nil
}

// UpdateRun implements the legacy db.RunStore path. Strict managed callers
// use CommitManagedRun instead.
func (s *FakeManagedStore) UpdateRun(_ context.Context, output *session.RunOutput, expectedStatus session.RunStatus) error {
	if output == nil || output.RunID == "" || !execution.ValidNamespace(output.Namespace) {
		return fmt.Errorf("invalid run")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := managedKey(output.Namespace, output.RunID)
	stored, exists := s.runs[key]
	if !exists {
		return db.ErrNotFound
	}
	if stored.output.Status != expectedStatus {
		return db.ErrConflict
	}
	if !expectedStatus.CanTransitionTo(output.Status) {
		return db.ErrInvalidTransition
	}
	clone, err := cloneOutput(output)
	if err != nil {
		return err
	}
	stored.output = clone
	stored.revision++
	s.runs[key] = stored
	return nil
}

func (s *FakeManagedStore) authorizeLocked(scope execution.NamespaceScope, claim *run.Claim) (*leaseState, error) {
	if claim == nil || !scope.HasNamespace() || claim.Namespace != scope.Namespace || claim.RunID == "" || claim.WorkerID == "" || claim.FencingToken == 0 {
		return nil, run.ErrInvalidClaim
	}
	state := s.leases[managedKey(scope.Namespace, claim.RunID)]
	if state == nil || state.claim.WorkerID != claim.WorkerID || state.claim.FencingToken != claim.FencingToken {
		return nil, run.ErrStaleFencingToken
	}
	if !s.clock().Before(state.claim.LeaseExpires) {
		return nil, run.ErrLeaseExpired
	}
	return state, nil
}

func cloneClaim(claim *run.Claim) *run.Claim {
	if claim == nil {
		return nil
	}
	clone := *claim
	return &clone
}

func cloneOutput(output *session.RunOutput) (*session.RunOutput, error) {
	if output == nil {
		return nil, nil
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	var clone session.RunOutput
	if err := json.Unmarshal(raw, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}
