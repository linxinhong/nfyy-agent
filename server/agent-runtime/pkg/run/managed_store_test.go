package run_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run/runtest"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestFakeManagedStoreContract(t *testing.T) {
	runtest.ManagedStoreContract(t, func(_ *testing.T, clock run.Clock) run.ManagedStore {
		return runtest.NewFakeManagedStore(clock)
	})
}

func TestRequireManagedStoreRejectsMissingCapability(t *testing.T) {
	t.Run("nil interface", func(t *testing.T) {
		var store db.RunStore
		if _, err := run.RequireManagedStore(store); !errors.Is(err, run.ErrManagedStoreRequired) {
			t.Fatalf("RequireManagedStore nil store: %v", err)
		}
	})
	t.Run("typed nil", func(t *testing.T) {
		var store *runtest.FakeManagedStore
		if _, err := run.RequireManagedStore(store); !errors.Is(err, run.ErrManagedStoreRequired) {
			t.Fatalf("RequireManagedStore typed-nil store: %v", err)
		}
	})
	t.Run("legacy", func(t *testing.T) {
		if _, err := run.RequireManagedStore(legacyRunStore{}); !errors.Is(err, run.ErrManagedStoreRequired) {
			t.Fatalf("RequireManagedStore legacy store: %v", err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		store := runtest.NewFakeManagedStore(nil)
		if got, err := run.RequireManagedStore(store); err != nil || got != store {
			t.Fatalf("RequireManagedStore managed store: got=%T err=%v", got, err)
		}
	})
}

func TestFakeManagedStoreUnknownHeartbeatDoesNotAuthorizeCommit(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	store := runtest.NewFakeManagedStore(func() time.Time { return now })
	scope := execution.NamespaceScope{Namespace: "contract"}
	claim, err := store.Claim(context.Background(), scope, "run-unknown-heartbeat", run.WorkerIdentity{WorkerID: "worker-a"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	unknown := errors.New("heartbeat outcome unknown")
	store.FailNextHeartbeat(unknown)
	if err := store.Heartbeat(context.Background(), scope, claim, time.Minute); !errors.Is(err, unknown) {
		t.Fatalf("heartbeat: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := store.CommitManagedRun(context.Background(), scope, claim, run.ManagedRunCommit{
		Run: &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
	}); !errors.Is(err, run.ErrLeaseExpired) {
		t.Fatalf("commit after unknown heartbeat and authoritative expiry: %v", err)
	}
}

func TestFakeManagedStoreRejectsNonPositiveHeartbeatWithoutExtendingLease(t *testing.T) {
	now := time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC)
	store := runtest.NewFakeManagedStore(func() time.Time { return now })
	scope := execution.NamespaceScope{Namespace: "contract"}
	claim, err := store.Claim(context.Background(), scope, "run-invalid-heartbeat", run.WorkerIdentity{WorkerID: "worker-a"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if err := store.Heartbeat(context.Background(), scope, claim, 0); !errors.Is(err, run.ErrInvalidClaim) {
		t.Fatalf("zero lease heartbeat: %v", err)
	}
	if err := store.Heartbeat(context.Background(), scope, claim, -time.Second); !errors.Is(err, run.ErrInvalidClaim) {
		t.Fatalf("negative lease heartbeat: %v", err)
	}
	now = now.Add(30 * time.Second)
	if _, err := store.CommitManagedRun(context.Background(), scope, claim, run.ManagedRunCommit{
		Run: &session.RunOutput{RunID: claim.RunID, Namespace: scope.Namespace, Status: session.RunStatusRunning},
	}); !errors.Is(err, run.ErrLeaseExpired) {
		t.Fatalf("commit at original expiry after invalid heartbeat: %v", err)
	}
}

type legacyRunStore struct{}

func (legacyRunStore) GetRun(context.Context, db.RunQuery) (*session.RunOutput, error) {
	return nil, db.ErrNotFound
}
func (legacyRunStore) AppendRun(context.Context, *session.RunOutput) error { return nil }
func (legacyRunStore) UpdateRun(context.Context, *session.RunOutput, session.RunStatus) error {
	return nil
}
