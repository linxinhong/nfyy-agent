package run

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

var (
	// ErrAdmissionQueueFull reports that a bounded admission queue cannot
	// accept another waiter. No execution goroutine is started for a rejected
	// request.
	ErrAdmissionQueueFull = errors.New("run admission queue is full")
	// ErrAdmissionTimeout reports that a caller waited longer than the
	// controller's configured bounded wait.
	ErrAdmissionTimeout = errors.New("run admission wait timed out")
	// ErrAdmissionPermitTenantMismatch reports that a context tries to reuse an
	// active permit for a different trusted namespace.
	ErrAdmissionPermitTenantMismatch = errors.New("run admission permit tenant mismatch")
)

// AdmissionConfig bounds runs sharing one runtime process. The local limits
// neither coordinate nor claim capacity across replicas; set Cluster to also
// enforce limits shared by every replica. A nil AdmissionController keeps
// the historical unlimited shared behavior.
type AdmissionConfig struct {
	// Total is the maximum number of active runs across all callers.
	Total int
	// Queue is the maximum number of callers allowed to wait for a permit.
	Queue int
	// PerTenant is the maximum number of active runs for one trusted namespace.
	// Legacy callers (without a namespace) consume only Total.
	PerTenant int
	// WaitTimeout bounds queue residence. Zero selects DefaultAdmissionWaitTimeout.
	WaitTimeout time.Duration
	// Cluster, when set, must also grant a cluster-wide slot before a run
	// starts. It is claimed before the local permit, so waiting for the
	// cluster holds no local capacity.
	Cluster ClusterAdmission
}

const DefaultAdmissionWaitTimeout = 30 * time.Second

// Validate rejects a controller which would silently turn a zero value into a
// reject-all limit. A caller that wants no shared limit leaves the controller nil.
func (c AdmissionConfig) Validate() error {
	if c.Total <= 0 || c.Queue < 0 || c.PerTenant <= 0 || c.WaitTimeout < 0 {
		return errors.New("run admission limits must have positive total and per-tenant limits and non-negative queue/wait timeout")
	}
	return nil
}

func (c AdmissionConfig) resolve() AdmissionConfig {
	if c.WaitTimeout == 0 {
		c.WaitTimeout = DefaultAdmissionWaitTimeout
	}
	return c
}

// AdmissionStats exposes process-local admission counters for diagnostics.
// Rejected is cumulative for the lifetime of the controller.
type AdmissionStats struct {
	Active   int
	Queued   int
	Rejected uint64
}

// AdmissionController is one shared, bounded run-admission point. It starts
// no goroutines: queued callers wait in their own calling goroutine until a
// permit is granted, cancelled, or times out.
type AdmissionController struct {
	cfg AdmissionConfig

	mu       sync.Mutex
	active   int
	tenants  map[string]int
	waiters  []*admissionWaiter
	queued   int
	rejected uint64
	// clusterWaiting counts callers polling Cluster for a slot.
	clusterWaiting int
}

type admissionWaiter struct {
	tenant string
	grant  chan *AdmissionPermit
	queued bool
	index  int
}

type admissionContextKey struct{}

// AdmissionPermit represents one active slot. Release is idempotent; a
// released permit is never accepted as an active context permit again.
type AdmissionPermit struct {
	controller *AdmissionController
	tenant     string
	released   bool // protected by controller.mu
	// clusterRelease returns the cluster slot; set before the permit is
	// shared and called once on Release.
	clusterRelease func()
}

// NewAdmissionController constructs an optional process-local controller.
func NewAdmissionController(cfg AdmissionConfig) (*AdmissionController, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &AdmissionController{cfg: cfg.resolve(), tenants: make(map[string]int)}, nil
}

// Acquire obtains a permit before work starts. A permit already present in ctx
// for this controller is borrowed, not transferred: it returns a nil release
// handle so only its original acquirer can return capacity. This prevents a
// Manager calling an Agent from both double-counting and releasing the
// Manager's capacity before the full executor exits.
func (c *AdmissionController) Acquire(ctx context.Context) (context.Context, *AdmissionPermit, error) {
	return c.acquire(ctx, false)
}

// AcquireRoot obtains a new permit for work that outlives its caller. It is
// used by Manager.StartContext: a background run detaches from its parent and
// therefore must not borrow a permit that the parent can release first.
func (c *AdmissionController) AcquireRoot(ctx context.Context) (context.Context, *AdmissionPermit, error) {
	return c.acquire(ctx, true)
}

func (c *AdmissionController) acquire(ctx context.Context, root bool) (context.Context, *AdmissionPermit, error) {
	if c == nil {
		return ctx, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	tenant, tenantErr := trustedAdmissionTenant(ctx)
	if tenantErr != nil {
		return nil, nil, tenantErr
	}
	if !root {
		if existing, ok := ctx.Value(admissionContextKey{}).(*AdmissionPermit); ok && existing != nil && existing.controller == c {
			c.mu.Lock()
			active := !existing.released
			c.mu.Unlock()
			if active {
				if existing.tenant != tenant {
					return nil, nil, ErrAdmissionPermitTenantMismatch
				}
				return ctx, nil, nil
			}
		}
	}

	if c.cfg.Cluster != nil {
		started := time.Now()
		clusterRelease, err := c.acquireCluster(ctx, tenant)
		recordAdmission(ctx, "cluster", started, err)
		if err != nil {
			return nil, nil, err
		}
		next, permit, err := c.acquireLocalTimed(ctx, tenant)
		if err != nil {
			clusterRelease()
			return nil, nil, err
		}
		permit.clusterRelease = clusterRelease
		return next, permit, nil
	}
	return c.acquireLocalTimed(ctx, tenant)
}

func (c *AdmissionController) acquireLocalTimed(ctx context.Context, tenant string) (context.Context, *AdmissionPermit, error) {
	started := time.Now()
	next, permit, err := c.acquireLocal(ctx, tenant)
	recordAdmission(ctx, "local", started, err)
	return next, permit, err
}

func (c *AdmissionController) acquireLocal(ctx context.Context, tenant string) (context.Context, *AdmissionPermit, error) {
	c.mu.Lock()
	if c.queued == 0 && c.canGrantLocked(tenant) {
		permit := c.grantLocked(tenant)
		c.mu.Unlock()
		return context.WithValue(ctx, admissionContextKey{}, permit), permit, nil
	}
	if c.queued >= c.cfg.Queue {
		c.rejected++
		c.mu.Unlock()
		return nil, nil, ErrAdmissionQueueFull
	}
	w := &admissionWaiter{tenant: tenant, grant: make(chan *AdmissionPermit, 1), queued: true}
	c.appendWaiterLocked(w)
	c.dispatchLocked()
	c.mu.Unlock()

	waitCtx := ctx
	var cancel context.CancelFunc
	if c.cfg.WaitTimeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, c.cfg.WaitTimeout)
		defer cancel()
	}
	select {
	case permit := <-w.grant:
		return context.WithValue(ctx, admissionContextKey{}, permit), permit, nil
	case <-waitCtx.Done():
		c.mu.Lock()
		if w.queued {
			c.removeWaiterLocked(w)
			c.dispatchLocked()
			c.mu.Unlock()
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) && c.cfg.WaitTimeout > 0 {
				return nil, nil, ErrAdmissionTimeout
			}
			return nil, nil, waitCtx.Err()
		}
		// A grant won the race with cancellation. It must be returned before
		// reporting cancellation so capacity cannot leak.
		c.mu.Unlock()
		select {
		case permit := <-w.grant:
			permit.Release()
		default:
		}
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) && c.cfg.WaitTimeout > 0 {
			return nil, nil, ErrAdmissionTimeout
		}
		return nil, nil, waitCtx.Err()
	}
}

// Release returns this permit once the actual executor exits. It is safe to
// call more than once, but only the first call returns capacity.
func (p *AdmissionPermit) Release() {
	if p == nil || p.controller == nil {
		return
	}
	if clusterRelease := p.releaseLocal(); clusterRelease != nil {
		// Outside the controller lock: cluster releases may do I/O.
		clusterRelease()
	}
}

// releaseLocal returns local capacity once and hands back the cluster
// release for the first caller only.
func (p *AdmissionPermit) releaseLocal() func() {
	c := p.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.released {
		return nil
	}
	p.released = true
	c.active--
	if p.tenant != "" {
		if c.tenants[p.tenant] <= 1 {
			delete(c.tenants, p.tenant)
		} else {
			c.tenants[p.tenant]--
		}
	}
	c.dispatchLocked()
	return p.clusterRelease
}

// Stats returns a consistent snapshot of controller state.
func (c *AdmissionController) Stats() AdmissionStats {
	if c == nil {
		return AdmissionStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return AdmissionStats{Active: c.active, Queued: c.queued, Rejected: c.rejected}
}

func trustedAdmissionTenant(ctx context.Context) (string, error) {
	namespace, ok := execution.NamespaceFromContext(ctx)
	if !ok {
		return "", nil
	}
	if err := execution.ValidateNamespace(namespace); err != nil {
		return "", err
	}
	if err := execution.ValidateNamespaceLength(namespace); err != nil {
		return "", err
	}
	return namespace, nil
}

func (c *AdmissionController) canGrantLocked(tenant string) bool {
	if c.active >= c.cfg.Total {
		return false
	}
	return tenant == "" || c.tenants[tenant] < c.cfg.PerTenant
}

func (c *AdmissionController) grantLocked(tenant string) *AdmissionPermit {
	c.active++
	if tenant != "" {
		c.tenants[tenant]++
	}
	return &AdmissionPermit{controller: c, tenant: tenant}
}

// dispatchLocked grants eligible waiters without letting a tenant at its cap
// block a later tenant. Tombstones make cancellation/removal O(1) while the
// contiguous queue traversal retains the original global earliest-eligible
// order and therefore FIFO within one tenant. This is process-local admission,
// not a cluster fairness guarantee.
func (c *AdmissionController) dispatchLocked() {
	for c.active < c.cfg.Total {
		var granted *admissionWaiter
		for _, w := range c.waiters {
			if w == nil {
				continue
			}
			if !c.canGrantLocked(w.tenant) {
				continue
			}
			granted = w
			break
		}
		if granted == nil {
			return
		}
		permit := c.grantLocked(granted.tenant)
		c.unlinkWaiterLocked(granted)
		granted.grant <- permit
	}
}

func (c *AdmissionController) removeWaiterLocked(target *admissionWaiter) {
	if target != nil && target.queued && target.index >= 0 && target.index < len(c.waiters) && c.waiters[target.index] == target {
		c.unlinkWaiterLocked(target)
	}
}

func (c *AdmissionController) appendWaiterLocked(waiter *admissionWaiter) {
	if len(c.waiters) == c.cfg.Queue {
		c.compactWaitersLocked()
	}
	waiter.index = len(c.waiters)
	waiter.queued = true
	c.waiters = append(c.waiters, waiter)
	c.queued++
}

func (c *AdmissionController) unlinkWaiterLocked(waiter *admissionWaiter) {
	c.waiters[waiter.index] = nil
	waiter.index = -1
	waiter.queued = false
	c.queued--
	// No future dispatch should pay to scan a full historical tombstone slice
	// after the logical queue drains. Keep the bounded backing storage for a
	// later burst, but reset its visible length so the uncontended path remains
	// O(1) after cancellations or grants empty the queue.
	if c.queued == 0 {
		c.waiters = c.waiters[:0]
	}
}

// compactWaitersLocked is called only when a bounded backing slice is full
// but its live queue is not. It clears tombstoned references, preserves live
// arrival order, and keeps storage bounded by cfg.Queue.
func (c *AdmissionController) compactWaitersLocked() {
	if c.queued == len(c.waiters) {
		return
	}
	n := 0
	for _, waiter := range c.waiters {
		if waiter == nil {
			continue
		}
		waiter.index = n
		c.waiters[n] = waiter
		n++
	}
	clear(c.waiters[n:])
	c.waiters = c.waiters[:n]
}
