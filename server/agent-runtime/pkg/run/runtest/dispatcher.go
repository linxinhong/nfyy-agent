package runtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
)

// DispatcherFactory constructs a dispatcher using the supplied clock.
type DispatcherFactory func(clock run.Clock) run.Dispatcher

// DispatcherContract verifies the common lease lifecycle required from
// process-local and durable Dispatcher implementations. Durable adapters must
// additionally test generation continuity after reopening their storage.
func DispatcherContract(t *testing.T, factory DispatcherFactory) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	dispatcher := factory(func() time.Time { return now })
	scope := execution.NamespaceScope{Namespace: "dispatcher-contract-" + uuid.NewString()}
	lease := 30 * time.Second
	runID := "run-lifecycle"
	workerA := run.WorkerIdentity{WorkerID: "worker-a"}
	workerB := run.WorkerIdentity{WorkerID: "worker-b"}
	workerC := run.WorkerIdentity{WorkerID: "worker-c"}

	first, err := dispatcher.Claim(ctx, scope, runID, workerA, lease)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	refreshed, err := dispatcher.Claim(ctx, scope, runID, workerA, lease)
	if err != nil || refreshed.FencingToken != first.FencingToken {
		t.Fatalf("live same-worker claim = %+v, err=%v; want generation %d", refreshed, err, first.FencingToken)
	}
	if _, err := dispatcher.Claim(ctx, scope, runID, workerB, lease); !errors.Is(err, run.ErrLeaseHeld) {
		t.Fatalf("live second-owner claim: %v", err)
	}

	// Expiry is inclusive: at LeaseExpires the old generation no longer owns
	// the run, even if the same worker ID is later reused.
	now = now.Add(lease)
	second, err := dispatcher.Claim(ctx, scope, runID, workerB, lease)
	if err != nil {
		t.Fatalf("expired takeover: %v", err)
	}
	if second.FencingToken <= first.FencingToken {
		t.Fatalf("takeover generation = %d, want > %d", second.FencingToken, first.FencingToken)
	}
	if err := dispatcher.Heartbeat(ctx, scope, first, 5*lease); !errors.Is(err, run.ErrStaleFencingToken) {
		t.Fatalf("old heartbeat after takeover: %v", err)
	}
	if err := dispatcher.Release(ctx, scope, first); !errors.Is(err, run.ErrStaleFencingToken) {
		t.Fatalf("old release after takeover: %v", err)
	}
	secondAgain, err := dispatcher.Claim(ctx, scope, runID, workerB, lease)
	if err != nil || secondAgain.FencingToken != second.FencingToken {
		t.Fatalf("new owner changed after stale operations: claim=%+v err=%v", secondAgain, err)
	}

	// If A's stale heartbeat had extended B's row, or stale release had
	// deleted it, C would either be blocked or observe a reset generation.
	now = now.Add(lease)
	third, err := dispatcher.Claim(ctx, scope, runID, workerC, lease)
	if err != nil {
		t.Fatalf("takeover after new owner's original expiry: %v", err)
	}
	if third.FencingToken <= second.FencingToken {
		t.Fatalf("post-stale-operation generation = %d, want > %d", third.FencingToken, second.FencingToken)
	}
	if err := dispatcher.Heartbeat(ctx, scope, second, lease); !errors.Is(err, run.ErrStaleFencingToken) {
		t.Fatalf("superseded owner heartbeat: %v", err)
	}
	if err := dispatcher.Release(ctx, scope, second); !errors.Is(err, run.ErrStaleFencingToken) {
		t.Fatalf("superseded owner release: %v", err)
	}

	if err := dispatcher.Release(ctx, scope, third); err != nil {
		t.Fatalf("release current owner: %v", err)
	}
	sameWorkerReclaim, err := dispatcher.Claim(ctx, scope, runID, workerC, lease)
	if err != nil {
		t.Fatalf("same-worker reclaim after release: %v", err)
	}
	if sameWorkerReclaim.FencingToken <= third.FencingToken {
		t.Fatalf("same-worker release/reclaim generation = %d, want > %d", sameWorkerReclaim.FencingToken, third.FencingToken)
	}
	if err := dispatcher.Heartbeat(ctx, scope, third, lease); !errors.Is(err, run.ErrStaleFencingToken) {
		t.Fatalf("released generation heartbeat after reclaim: %v", err)
	}
	if err := dispatcher.Release(ctx, scope, third); !errors.Is(err, run.ErrStaleFencingToken) {
		t.Fatalf("released generation release after reclaim: %v", err)
	}

	now = now.Add(lease)
	sameWorkerAfterExpiry, err := dispatcher.Claim(ctx, scope, runID, workerC, lease)
	if err != nil {
		t.Fatalf("same-worker reclaim after expiry: %v", err)
	}
	if sameWorkerAfterExpiry.FencingToken <= sameWorkerReclaim.FencingToken {
		t.Fatalf("same-worker expiry generation = %d, want > %d", sameWorkerAfterExpiry.FencingToken, sameWorkerReclaim.FencingToken)
	}

	identityScopeA := execution.NamespaceScope{Namespace: "dispatcher-identity-a-" + uuid.NewString()}
	identityScopeB := execution.NamespaceScope{Namespace: "dispatcher-identity-b-" + uuid.NewString()}
	identityRunID := "same-run-id"
	identityWorker := run.WorkerIdentity{WorkerID: "same-worker-id"}
	claimA, err := dispatcher.Claim(ctx, identityScopeA, identityRunID, identityWorker, lease)
	if err != nil {
		t.Fatalf("claim namespace A: %v", err)
	}
	claimB, err := dispatcher.Claim(ctx, identityScopeB, identityRunID, identityWorker, lease)
	if err != nil {
		t.Fatalf("claim namespace B: %v", err)
	}
	invalidClaims := []struct {
		name  string
		scope execution.NamespaceScope
		claim *run.Claim
	}{
		{name: "nil claim", scope: identityScopeB},
		{name: "invalid scope", scope: execution.NamespaceScope{}, claim: claimB},
		{name: "namespace mismatch", scope: identityScopeB, claim: claimA},
		{name: "empty run", scope: identityScopeB, claim: cloneClaimWith(claimB, func(claim *run.Claim) { claim.RunID = "" })},
		{name: "empty worker", scope: identityScopeB, claim: cloneClaimWith(claimB, func(claim *run.Claim) { claim.WorkerID = "" })},
		{name: "zero generation", scope: identityScopeB, claim: cloneClaimWith(claimB, func(claim *run.Claim) { claim.FencingToken = 0 })},
	}
	for _, test := range invalidClaims {
		if err := dispatcher.Heartbeat(ctx, test.scope, test.claim, 5*lease); !errors.Is(err, run.ErrInvalidClaim) {
			t.Errorf("%s heartbeat: %v", test.name, err)
		}
		if err := dispatcher.Release(ctx, test.scope, test.claim); !errors.Is(err, run.ErrInvalidClaim) {
			t.Errorf("%s release: %v", test.name, err)
		}
	}
	if err := dispatcher.Heartbeat(ctx, identityScopeB, claimB, 0); !errors.Is(err, run.ErrInvalidClaim) {
		t.Errorf("zero lease heartbeat: %v", err)
	}
	if err := dispatcher.Heartbeat(ctx, identityScopeB, claimB, -time.Second); !errors.Is(err, run.ErrInvalidClaim) {
		t.Errorf("negative lease heartbeat: %v", err)
	}
	if _, err := dispatcher.Claim(ctx, identityScopeB, identityRunID, run.WorkerIdentity{WorkerID: "other-worker"}, lease); !errors.Is(err, run.ErrLeaseHeld) {
		t.Fatalf("invalid operation released namespace B lease: %v", err)
	}
	now = now.Add(lease)
	claimBAfterExpiry, err := dispatcher.Claim(ctx, identityScopeB, identityRunID, run.WorkerIdentity{WorkerID: "other-worker"}, lease)
	if err != nil {
		t.Fatalf("namespace B takeover at original expiry: %v", err)
	}
	if claimBAfterExpiry.FencingToken <= claimB.FencingToken {
		t.Fatalf("namespace B generation after invalid operations = %d, want > %d", claimBAfterExpiry.FencingToken, claimB.FencingToken)
	}
}

func cloneClaimWith(claim *run.Claim, mutate func(*run.Claim)) *run.Claim {
	clone := *claim
	mutate(&clone)
	return &clone
}
