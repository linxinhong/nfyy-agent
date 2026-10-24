package run

import (
	"context"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

// Clock is an injectable time source. Lease and expiry tests inject a fake
// Clock instead of sleeping (RC-08 constraint).
type Clock func() time.Time

// NowClock is the default wall clock, always in UTC.
func NowClock() time.Time { return time.Now().UTC() }

// WorkerIdentity identifies one worker process/instance.
type WorkerIdentity struct {
	WorkerID string
}

// Claim is a leased ownership over one run. It carries the per-run
// monotonically increasing fencing token; every takeover produces a strictly
// greater token, and stale tokens are rejected on heartbeat/release.
type Claim struct {
	RunID        string
	Namespace    string
	WorkerID     string
	FencingToken uint64
	LeaseExpires time.Time
}

// Sentinel errors for the dispatcher.
var (
	// ErrLeaseHeld reports that a live lease for the run is held by a
	// different worker.
	ErrLeaseHeld = errors.New("run: run lease is held by another worker")
)

// Dispatcher coordinates cross-instance run ownership via leases and
// per-run fencing tokens. Concrete stores provide process-local (in-memory,
// SQLite) and multi-instance (PostgreSQL) implementations.
type Dispatcher interface {
	// Claim acquires ownership of a run. A live lease held by another
	// worker fails with ErrLeaseHeld; an expired lease is taken over with a
	// strictly greater fencing token; re-claiming one's own live lease is
	// idempotent and refreshes it.
	Claim(ctx context.Context, scope execution.NamespaceScope, runID string, worker WorkerIdentity, lease time.Duration) (*Claim, error)

	// Heartbeat extends the lease. A claim whose fencing token no longer
	// matches (taken over or released) is rejected with ErrStaleFencingToken.
	Heartbeat(ctx context.Context, scope execution.NamespaceScope, claim *Claim, lease time.Duration) error

	// Release drops ownership. A stale claim is rejected with
	// ErrStaleFencingToken so a zombie worker cannot free a successor's lease.
	Release(ctx context.Context, scope execution.NamespaceScope, claim *Claim) error
}
