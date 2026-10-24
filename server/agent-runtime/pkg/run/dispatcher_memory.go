package run

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// MemoryDispatcher is the single-process reference implementation of
// Dispatcher. It does not provide cross-worker atomicity (that is
// PostgreSQL's job in managed multi-worker mode) but exercises the full
// lease/fencing contract with an injectable clock. Its generation history is
// process-local: constructing a new MemoryDispatcher starts a new history;
// callers that require continuity across restarts must use a durable adapter.
type MemoryDispatcher struct {
	mu     sync.Mutex
	leases map[string]*Claim // includes released tombstones; key: namespace\x00runID
	runs   map[string]memoryRun
	clock  Clock
}

type memoryRun struct {
	output   *session.RunOutput
	revision uint64
}

// NewMemoryDispatcher creates an in-memory dispatcher.
func NewMemoryDispatcher() *MemoryDispatcher {
	return &MemoryDispatcher{
		leases: make(map[string]*Claim),
		runs:   make(map[string]memoryRun),
		clock:  NowClock,
	}
}

var _ ManagedStore = (*MemoryDispatcher)(nil)

// SetClock injects a clock for lease-expiry tests.
func (d *MemoryDispatcher) SetClock(clock Clock) *MemoryDispatcher {
	if clock != nil {
		d.clock = clock
	}
	return d
}

func leaseKey(scope execution.NamespaceScope, runID string) string {
	ns := scope.Namespace
	if ns == "" {
		ns = execution.ReservedNamespace
	}
	return ns + "\x00" + runID
}

// Claim implements Dispatcher.
func (d *MemoryDispatcher) Claim(ctx context.Context, scope execution.NamespaceScope, runID string, worker WorkerIdentity, lease time.Duration) (*Claim, error) {
	if !scope.HasNamespace() || runID == "" || worker.WorkerID == "" || lease <= 0 {
		return nil, ErrInvalidClaim
	}
	now := d.clock()
	expires := now.Add(lease)
	d.mu.Lock()
	defer d.mu.Unlock()
	key := leaseKey(scope, runID)
	if existing, ok := d.leases[key]; ok {
		if now.Before(existing.LeaseExpires) {
			if existing.WorkerID != worker.WorkerID {
				return nil, ErrLeaseHeld
			}
			existing.LeaseExpires = expires
			return cloneClaim(existing), nil
		}
		// Expired or released: reclaim with a strictly greater fencing token,
		// including when the worker ID happens to be reused.
		claim := &Claim{RunID: runID, Namespace: scope.Namespace, WorkerID: worker.WorkerID, FencingToken: FencingToken(existing.FencingToken), LeaseExpires: expires}
		d.leases[key] = claim
		return cloneClaim(claim), nil
	}
	claim := &Claim{RunID: runID, Namespace: scope.Namespace, WorkerID: worker.WorkerID, FencingToken: 1, LeaseExpires: expires}
	d.leases[key] = claim
	return cloneClaim(claim), nil
}

// Heartbeat implements Dispatcher.
func (d *MemoryDispatcher) Heartbeat(ctx context.Context, scope execution.NamespaceScope, claim *Claim, lease time.Duration) error {
	if !validLeaseClaim(scope, claim) || lease <= 0 {
		return ErrInvalidClaim
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	existing, ok := d.leases[leaseKey(scope, claim.RunID)]
	if !ok || existing.WorkerID != claim.WorkerID || existing.FencingToken != claim.FencingToken {
		return ErrStaleFencingToken
	}
	now := d.clock()
	if !now.Before(existing.LeaseExpires) {
		return ErrLeaseExpired
	}
	existing.LeaseExpires = now.Add(lease)
	return nil
}

// Release implements Dispatcher.
func (d *MemoryDispatcher) Release(ctx context.Context, scope execution.NamespaceScope, claim *Claim) error {
	if !validLeaseClaim(scope, claim) {
		return ErrInvalidClaim
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := leaseKey(scope, claim.RunID)
	existing, ok := d.leases[key]
	if !ok || existing.WorkerID != claim.WorkerID || existing.FencingToken != claim.FencingToken {
		return ErrStaleFencingToken
	}
	if !d.clock().Before(existing.LeaseExpires) {
		return ErrLeaseExpired
	}
	// Retain the last generation as a tombstone so release/reclaim cannot
	// recreate generation 1 within this dispatcher instance.
	d.leases[key] = &Claim{RunID: claim.RunID, Namespace: scope.Namespace, FencingToken: existing.FencingToken}
	return nil
}

// GetManagedRun returns a cloned run together with its independent content
// revision. Lease generation is deliberately not exposed as that revision.
func (d *MemoryDispatcher) GetManagedRun(_ context.Context, scope execution.NamespaceScope, runID string) (*ManagedRunSnapshot, error) {
	if !scope.HasNamespace() || runID == "" {
		return nil, ErrInvalidClaim
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	stored, ok := d.runs[leaseKey(scope, runID)]
	if !ok {
		return nil, db.ErrNotFound
	}
	output, err := cloneMemoryRun(stored.output)
	if err != nil {
		return nil, err
	}
	return &ManagedRunSnapshot{Run: output, Revision: stored.revision}, nil
}

// CommitManagedRun validates the authoritative lease and applies the Run CAS
// while holding the same mutex. This is process-local atomicity; callers that
// need restart or cross-process guarantees must use a durable adapter.
func (d *MemoryDispatcher) CommitManagedRun(_ context.Context, scope execution.NamespaceScope, claim *Claim, commit ManagedRunCommit) (uint64, error) {
	if commit.Run == nil || commit.Run.RunID == "" || commit.Run.Namespace != scope.Namespace ||
		claim == nil || claim.RunID != commit.Run.RunID {
		return 0, ErrInvalidClaim
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.authorizeManagedLocked(scope, claim); err != nil {
		return 0, err
	}
	key := leaseKey(scope, commit.Run.RunID)
	stored, exists := d.runs[key]
	if commit.ExpectedRevision == 0 && commit.ExpectedStatus == "" {
		if exists {
			return 0, db.ErrConflict
		}
		output, err := cloneMemoryRun(commit.Run)
		if err != nil {
			return 0, err
		}
		d.runs[key] = memoryRun{output: output, revision: 1}
		return 1, nil
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
	output, err := cloneMemoryRun(commit.Run)
	if err != nil {
		return 0, err
	}
	stored.output = output
	stored.revision++
	d.runs[key] = stored
	return stored.revision, nil
}

// GetRun implements the legacy db.RunStore path.
func (d *MemoryDispatcher) GetRun(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	if query.RunID == "" {
		return nil, fmt.Errorf("run_id is empty")
	}
	snapshot, err := d.GetManagedRun(ctx, execution.NamespaceScope{Namespace: query.Namespace}, query.RunID)
	if err == db.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if query.UserID != "" && snapshot.Run.UserID != query.UserID {
		return nil, nil
	}
	return snapshot.Run, nil
}

// AppendRun preserves the non-managed compatibility path.
func (d *MemoryDispatcher) AppendRun(_ context.Context, output *session.RunOutput) error {
	if output == nil || output.RunID == "" || !execution.ValidNamespace(output.Namespace) {
		return fmt.Errorf("invalid run")
	}
	clone, err := cloneMemoryRun(output)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := leaseKey(execution.NamespaceScope{Namespace: output.Namespace}, output.RunID)
	if _, exists := d.runs[key]; exists {
		return db.ErrConflict
	}
	d.runs[key] = memoryRun{output: clone, revision: 1}
	return nil
}

// UpdateRun preserves the non-managed compatibility path.
func (d *MemoryDispatcher) UpdateRun(_ context.Context, output *session.RunOutput, expectedStatus session.RunStatus) error {
	if output == nil || output.RunID == "" || !execution.ValidNamespace(output.Namespace) {
		return fmt.Errorf("invalid run")
	}
	if !expectedStatus.CanTransitionTo(output.Status) {
		return db.ErrInvalidTransition
	}
	clone, err := cloneMemoryRun(output)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := leaseKey(execution.NamespaceScope{Namespace: output.Namespace}, output.RunID)
	stored, exists := d.runs[key]
	if !exists {
		return db.ErrNotFound
	}
	if stored.output.Status != expectedStatus {
		return db.ErrConflict
	}
	stored.output = clone
	stored.revision++
	d.runs[key] = stored
	return nil
}

func (d *MemoryDispatcher) authorizeManagedLocked(scope execution.NamespaceScope, claim *Claim) error {
	if !validLeaseClaim(scope, claim) {
		return ErrInvalidClaim
	}
	existing, ok := d.leases[leaseKey(scope, claim.RunID)]
	if !ok || existing.WorkerID != claim.WorkerID || existing.FencingToken != claim.FencingToken {
		return ErrStaleFencingToken
	}
	if !d.clock().Before(existing.LeaseExpires) {
		return ErrLeaseExpired
	}
	return nil
}

func cloneMemoryRun(output *session.RunOutput) (*session.RunOutput, error) {
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

func validLeaseClaim(scope execution.NamespaceScope, claim *Claim) bool {
	return scope.HasNamespace() && claim != nil && claim.Namespace == scope.Namespace &&
		claim.RunID != "" && claim.WorkerID != "" && claim.FencingToken != 0
}

func cloneClaim(c *Claim) *Claim {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}
