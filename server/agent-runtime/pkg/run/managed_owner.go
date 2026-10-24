package run

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

var (
	// ErrOwnershipLost reports that a managed execution can no longer prove
	// that it owns the run. The underlying dispatcher error is joined to it.
	ErrOwnershipLost = errors.New("run: managed execution ownership lost")
	// ErrManagedCommitRequired reports that a managed executor returned
	// without first creating its root Run through the claim-bound committer.
	ErrManagedCommitRequired = errors.New("run: managed executor did not create a claim-bound run")
	// ErrManagedExecutorIncomplete reports an executor that returned while its
	// durable Run was still pending/running.
	ErrManagedExecutorIncomplete = errors.New("run: managed executor returned without a paused or terminal run")
	// ErrManagedLocalClaimRequired rejects strict cancellation when this
	// Manager does not hold the active claim in its own process.
	ErrManagedLocalClaimRequired = errors.New("run: managed cancellation requires a local active claim")
)

// ManagedConfig configures one strict managed execution. Store owns both the
// lease and Run state; the optional timing fields exist for deterministic
// fault-injection tests and normally use finite defaults derived from Lease.
type ManagedConfig struct {
	Store             ManagedStore
	Scope             execution.NamespaceScope
	Worker            WorkerIdentity
	Lease             time.Duration
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	Clock             Clock
}

type managedOwnerContextKey struct{}

// ManagedRunOwner holds one live claim and is the only claim-bound commit
// path installed in a strict executor context. It is reusable by managed
// initial execution and managed recovery.
type ManagedRunOwner struct {
	store  ManagedStore
	scope  execution.NamespaceScope
	worker WorkerIdentity
	lease  time.Duration
	clock  Clock

	ctx    context.Context
	cancel context.CancelCauseFunc
	claim  *Claim

	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
	heartbeatStop     chan struct{}
	heartbeatDone     chan struct{}
	stopOnce          sync.Once

	stateMu        sync.RWMutex
	confirmedUntil time.Time
	ownershipErr   error
	lastHBErr      error

	commitMu   sync.Mutex
	revision   uint64
	status     session.RunStatus
	created    bool
	releaseMu  sync.Mutex
	released   bool
	releaseErr error
}

// AcquireManagedRunOwner claims a run before any executor work starts and
// starts its bounded heartbeat lifecycle.
func AcquireManagedRunOwner(parent context.Context, runID string, cfg ManagedConfig) (*ManagedRunOwner, error) {
	if cfg.Store == nil || isNilCapability(cfg.Store) {
		return nil, ErrManagedStoreRequired
	}
	if runID == "" || cfg.Worker.WorkerID == "" || cfg.Lease <= 0 ||
		execution.ValidateNamespace(cfg.Scope.Namespace) != nil ||
		execution.ValidateNamespaceLength(cfg.Scope.Namespace) != nil {
		return nil, ErrInvalidClaim
	}
	resolved, err := db.NamespaceFor(parent, cfg.Scope.Namespace)
	if err != nil {
		return nil, err
	}
	if resolved != cfg.Scope.Namespace {
		return nil, db.ErrNotFound
	}
	if cfg.Clock == nil {
		cfg.Clock = NowClock
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = cfg.Lease / 3
	}
	if cfg.HeartbeatInterval <= 0 || cfg.HeartbeatInterval >= cfg.Lease {
		return nil, ErrInvalidClaim
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = cfg.HeartbeatInterval
	}
	if cfg.HeartbeatTimeout <= 0 || cfg.HeartbeatTimeout >= cfg.Lease {
		return nil, ErrInvalidClaim
	}

	claim, err := cfg.Store.Claim(parent, cfg.Scope, runID, cfg.Worker, cfg.Lease)
	if err != nil {
		return nil, err
	}
	if claim == nil || claim.RunID != runID || claim.Namespace != cfg.Scope.Namespace ||
		claim.WorkerID != cfg.Worker.WorkerID || claim.FencingToken == 0 || claim.LeaseExpires.IsZero() {
		return nil, ErrInvalidClaim
	}
	base, cancel := context.WithCancelCause(execution.ContextWithNamespace(parent, cfg.Scope.Namespace))
	owner := &ManagedRunOwner{
		store: cfg.Store, scope: cfg.Scope, worker: cfg.Worker, lease: cfg.Lease, clock: cfg.Clock,
		cancel: cancel, claim: cloneClaim(claim), confirmedUntil: claim.LeaseExpires,
		heartbeatInterval: cfg.HeartbeatInterval, heartbeatTimeout: cfg.HeartbeatTimeout,
		heartbeatStop: make(chan struct{}), heartbeatDone: make(chan struct{}),
	}
	owner.ctx = context.WithValue(base, managedOwnerContextKey{}, owner)
	go owner.heartbeatLoop()
	return owner, nil
}

// Context is the executor context carrying this claim-bound owner.
func (o *ManagedRunOwner) Context() context.Context { return o.ctx }

// ManagedRunOwnerFromContext returns the strict owner installed by a managed
// execution entry point. A managed root executor must use it for every Run
// write.
func ManagedRunOwnerFromContext(ctx context.Context) (*ManagedRunOwner, bool) {
	owner, ok := ctx.Value(managedOwnerContextKey{}).(*ManagedRunOwner)
	return owner, ok && owner != nil
}

// RunID is the only root Run this owner may commit.
func (o *ManagedRunOwner) RunID() string { return o.claim.RunID }

// Scope returns the immutable namespace scope bound into the claim.
func (o *ManagedRunOwner) Scope() execution.NamespaceScope { return o.scope }

// Claim returns a defensive copy for diagnostics and tests.
func (o *ManagedRunOwner) Claim() *Claim {
	o.stateMu.RLock()
	defer o.stateMu.RUnlock()
	return cloneClaim(o.claim)
}

// Cancel cooperatively stops the executor and any in-flight heartbeat.
func (o *ManagedRunOwner) Cancel(cause error) {
	if cause == nil {
		cause = context.Canceled
	}
	o.cancel(cause)
}

// OwnershipError reports a confirmed loss of ownership. Unknown heartbeat
// errors are not reported as loss until the last confirmed lease deadline.
func (o *ManagedRunOwner) OwnershipError() error {
	o.stateMu.RLock()
	defer o.stateMu.RUnlock()
	return o.ownershipErr
}

// Create atomically creates the managed root Run at revision 1.
func (o *ManagedRunOwner) Create(ctx context.Context, output *session.RunOutput) (uint64, error) {
	return o.commit(ctx, output, 0, "", false)
}

// Update atomically applies a revision/status CAS under the live claim.
// It retains the historical managed-owner behavior: caller cancellation and
// deadlines do not prevent a bounded terminal commit while the lease remains
// valid.
func (o *ManagedRunOwner) Update(ctx context.Context, output *session.RunOutput, expected session.RunStatus) (uint64, error) {
	return o.commit(ctx, output, o.currentRevision(), expected, false)
}

// UpdateBounded is the opt-in claim-bound update path for a caller with an
// explicit persistence deadline. It still detaches cancellation for durable
// terminal writes, but never extends the supplied deadline beyond the live
// lease cap. Legacy managed callers must continue to use Update.
func (o *ManagedRunOwner) UpdateBounded(ctx context.Context, output *session.RunOutput, expected session.RunStatus) (uint64, error) {
	return o.commit(ctx, output, o.currentRevision(), expected, true)
}

// Snapshot loads the authoritative Run and makes its revision/status the next
// expected commit point. Managed recovery calls this after acquiring a claim.
func (o *ManagedRunOwner) Snapshot(ctx context.Context) (*ManagedRunSnapshot, error) {
	o.commitMu.Lock()
	defer o.commitMu.Unlock()
	bounded, cancel, err := o.commitContext(ctx, false)
	if err != nil {
		return nil, err
	}
	defer cancel()
	snapshot, err := o.store.GetManagedRun(bounded, o.scope, o.claim.RunID)
	if err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.Run == nil {
		return nil, db.ErrNotFound
	}
	o.revision, o.status, o.created = snapshot.Revision, snapshot.Run.Status, true
	return snapshot, nil
}

func (o *ManagedRunOwner) commit(ctx context.Context, output *session.RunOutput, revision uint64, expected session.RunStatus, preserveDeadline bool) (uint64, error) {
	if output == nil || output.RunID != o.claim.RunID {
		return 0, ErrInvalidClaim
	}
	if output.Namespace == "" {
		output.Namespace = o.scope.Namespace
	}
	if output.Namespace != o.scope.Namespace {
		return 0, ErrInvalidClaim
	}
	o.commitMu.Lock()
	defer o.commitMu.Unlock()
	if err := o.OwnershipError(); err != nil {
		return 0, err
	}
	if expected == "" {
		if o.created || revision != 0 {
			return 0, db.ErrConflict
		}
	} else if !o.created || revision == 0 || revision != o.revision || expected != o.status {
		return 0, db.ErrConflict
	}
	bounded, cancel, err := o.commitContext(ctx, preserveDeadline)
	if err != nil {
		return 0, err
	}
	defer cancel()
	next, err := o.store.CommitManagedRun(bounded, o.scope, o.Claim(), ManagedRunCommit{
		Run: output, ExpectedRevision: revision, ExpectedStatus: expected,
	})
	if err != nil {
		if isDefinitiveOwnershipError(err) {
			o.loseOwnership(err)
		}
		return 0, err
	}
	o.revision, o.status, o.created = next, output.Status, true
	return next, nil
}

func (o *ManagedRunOwner) currentRevision() uint64 {
	o.commitMu.Lock()
	defer o.commitMu.Unlock()
	return o.revision
}

func (o *ManagedRunOwner) commitContext(ctx context.Context, preserveDeadline bool) (context.Context, context.CancelFunc, error) {
	if err := o.OwnershipError(); err != nil {
		return nil, nil, err
	}
	o.stateMu.RLock()
	remaining := o.confirmedUntil.Sub(o.clock())
	o.stateMu.RUnlock()
	if remaining <= 0 {
		err := errors.Join(ErrOwnershipLost, ErrLeaseExpired)
		o.loseOwnership(ErrLeaseExpired)
		return nil, nil, err
	}
	if remaining > 5*time.Second {
		remaining = 5 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Only explicitly bounded callers retain their deadline. Managed Update's
	// historical contract intentionally permits a finite terminal write after
	// the caller context has expired, so it must not inherit this behavior.
	if preserveDeadline {
		if deadline, ok := ctx.Deadline(); ok {
			if untilDeadline := time.Until(deadline); untilDeadline < remaining {
				remaining = untilDeadline
			}
		}
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), remaining)
	return bounded, cancel, nil
}

func (o *ManagedRunOwner) heartbeatLoop() {
	defer close(o.heartbeatDone)
	for {
		o.stateMu.RLock()
		remaining := o.confirmedUntil.Sub(o.clock())
		o.stateMu.RUnlock()
		if remaining <= 0 {
			o.expireOwnership()
			return
		}
		delay := o.heartbeatInterval
		if delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-o.ctx.Done():
			timer.Stop()
			return
		case <-o.heartbeatStop:
			timer.Stop()
			return
		case <-timer.C:
		}

		o.stateMu.RLock()
		remaining = o.confirmedUntil.Sub(o.clock())
		o.stateMu.RUnlock()
		if remaining <= 0 {
			o.expireOwnership()
			return
		}
		timeout := o.heartbeatTimeout
		if timeout > remaining {
			timeout = remaining
		}
		sentAt := o.clock()
		hbCtx, cancel := context.WithTimeout(o.ctx, timeout)
		err := o.store.Heartbeat(hbCtx, o.scope, o.claim, o.lease)
		cancel()
		if err == nil {
			confirmedUntil := sentAt.Add(o.lease)
			if !o.clock().Before(confirmedUntil) {
				o.loseOwnership(ErrLeaseExpired)
				return
			}
			o.stateMu.Lock()
			o.confirmedUntil = confirmedUntil
			o.claim.LeaseExpires = o.confirmedUntil
			o.lastHBErr = nil
			o.stateMu.Unlock()
			continue
		}
		if isDefinitiveOwnershipError(err) {
			o.loseOwnership(err)
			return
		}
		// Unknown infrastructure failures never extend confirmedUntil.
		o.stateMu.Lock()
		o.lastHBErr = err
		o.stateMu.Unlock()
	}
}

func (o *ManagedRunOwner) expireOwnership() {
	o.stateMu.RLock()
	last := o.lastHBErr
	o.stateMu.RUnlock()
	cause := error(ErrLeaseExpired)
	if last != nil {
		cause = errors.Join(ErrLeaseExpired, last)
	}
	o.loseOwnership(cause)
}

func (o *ManagedRunOwner) loseOwnership(cause error) {
	err := errors.Join(ErrOwnershipLost, cause)
	o.stateMu.Lock()
	if o.ownershipErr == nil {
		o.ownershipErr = err
	}
	err = o.ownershipErr
	o.stateMu.Unlock()
	o.cancel(err)
}

func isDefinitiveOwnershipError(err error) bool {
	return errors.Is(err, ErrStaleFencingToken) || errors.Is(err, ErrLeaseExpired) || errors.Is(err, ErrInvalidClaim)
}

// StopHeartbeat requests shutdown and waits for the heartbeat worker. The
// wait is bounded by ctx; compliant stores also receive bounded contexts for
// each heartbeat call.
func (o *ManagedRunOwner) StopHeartbeat(ctx context.Context) error {
	o.stopOnce.Do(func() { close(o.heartbeatStop) })
	select {
	case <-o.heartbeatDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release stops and joins heartbeats before dropping the claim. Callers must
// persist their terminal or paused state before invoking Release.
func (o *ManagedRunOwner) Release(ctx context.Context) error {
	if err := o.StopHeartbeat(ctx); err != nil {
		return err
	}
	o.releaseMu.Lock()
	defer o.releaseMu.Unlock()
	if o.released {
		return o.releaseErr
	}
	o.released = true
	o.releaseErr = o.store.Release(ctx, o.scope, o.claim)
	return o.releaseErr
}
