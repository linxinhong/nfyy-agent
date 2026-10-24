package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

func TestDispatcherClaimHeartbeatReleaseAndTakeover(t *testing.T) {
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}
	var now time.Time = time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	d := NewMemoryDispatcher().SetClock(func() time.Time { return now })

	lease := 30 * time.Second

	// Fresh claim owns the run with fencing token 1.
	c1, err := d.Claim(ctx, scope, "run-1", WorkerIdentity{WorkerID: "w1"}, lease)
	if err != nil || c1.FencingToken != 1 {
		t.Fatalf("first claim = %+v, err=%v", c1, err)
	}

	// Re-claiming one's own live lease is idempotent and refreshes.
	c1b, err := d.Claim(ctx, scope, "run-1", WorkerIdentity{WorkerID: "w1"}, lease)
	if err != nil || c1b.FencingToken != 1 || c1b.WorkerID != "w1" {
		t.Fatalf("re-claim own = %+v, err=%v", c1b, err)
	}

	// A different worker cannot claim a live lease.
	if _, err := d.Claim(ctx, scope, "run-1", WorkerIdentity{WorkerID: "w2"}, lease); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("live lease claim err = %v, want ErrLeaseHeld", err)
	}

	// Heartbeat extends the lease.
	if err := d.Heartbeat(ctx, scope, c1, lease); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	// After the lease expires, another worker takes over with a greater token.
	now = now.Add(lease + time.Second)
	c2, err := d.Claim(ctx, scope, "run-1", WorkerIdentity{WorkerID: "w2"}, lease)
	if err != nil || c2.FencingToken != 2 || c2.WorkerID != "w2" {
		t.Fatalf("takeover claim = %+v, err=%v", c2, err)
	}

	// The stale owner's heartbeat and release are rejected.
	if err := d.Heartbeat(ctx, scope, c1, lease); !errors.Is(err, ErrStaleFencingToken) {
		t.Fatalf("stale heartbeat err = %v, want ErrStaleFencingToken", err)
	}
	if err := d.Release(ctx, scope, c1); !errors.Is(err, ErrStaleFencingToken) {
		t.Fatalf("stale release err = %v, want ErrStaleFencingToken", err)
	}

	// The new owner can release cleanly.
	if err := d.Release(ctx, scope, c2); err != nil {
		t.Fatalf("release new owner: %v", err)
	}
}

func TestDispatcherNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	d := NewMemoryDispatcher()
	lease := time.Minute
	a := execution.NamespaceScope{Namespace: "ns-a"}
	b := execution.NamespaceScope{Namespace: "ns-b"}

	if _, err := d.Claim(ctx, a, "run-1", WorkerIdentity{WorkerID: "w1"}, lease); err != nil {
		t.Fatalf("claim ns-a: %v", err)
	}
	// Same run id in a different namespace is an independent lease.
	if _, err := d.Claim(ctx, b, "run-1", WorkerIdentity{WorkerID: "w1"}, lease); err != nil {
		t.Fatalf("claim ns-b: %v", err)
	}
}

func TestDispatcherRejectsUnscopedClaim(t *testing.T) {
	ctx := context.Background()
	d := NewMemoryDispatcher()
	if _, err := d.Claim(ctx, execution.NamespaceScope{}, "run-1", WorkerIdentity{WorkerID: "w1"}, time.Minute); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("unscoped claim err = %v, want fail-closed", err)
	}
}

func TestMemoryDispatcherGenerationIsProcessLocal(t *testing.T) {
	ctx := context.Background()
	scope := execution.NamespaceScope{Namespace: "ns"}
	first := NewMemoryDispatcher()
	claim, err := first.Claim(ctx, scope, "run-1", WorkerIdentity{WorkerID: "w1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx, scope, claim); err != nil {
		t.Fatal(err)
	}
	claimAfterProcessRestart, err := NewMemoryDispatcher().Claim(ctx, scope, "run-1", WorkerIdentity{WorkerID: "w2"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimAfterProcessRestart.FencingToken != 1 {
		t.Fatalf("new in-memory dispatcher generation = %d, want new process-local history at 1", claimAfterProcessRestart.FencingToken)
	}
}
