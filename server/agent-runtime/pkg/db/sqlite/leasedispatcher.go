package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
)

// LeaseDispatcher is the SQLite durable implementation of run.Dispatcher. It
// is single-process (managed multi-worker deployments must use the
// PostgreSQL adapter) and serializes claim decisions with a mutex.
type LeaseDispatcher struct {
	db    *sql.DB
	store *DB
	mu    sync.Mutex
	clock run.Clock
}

// NewLeaseDispatcher wraps an existing SQLite DB for lease management.
func NewLeaseDispatcher(d *DB) *LeaseDispatcher {
	return &LeaseDispatcher{db: d.db, store: d, clock: run.NowClock}
}

// SetClock injects a clock for lease-expiry tests.
func (d *LeaseDispatcher) SetClock(clock run.Clock) *LeaseDispatcher {
	if clock != nil {
		d.clock = clock
	}
	return d
}

var _ run.ManagedStore = (*LeaseDispatcher)(nil)

func (d *LeaseDispatcher) load(ctx context.Context, namespace, runID string) (workerID string, token int64, expires time.Time, expiresRaw string, found bool, err error) {
	err = d.db.QueryRowContext(ctx, `
		SELECT worker_id, fencing_token, lease_expires_at
		FROM dispatch_leases WHERE namespace = ? AND run_id = ?`,
		namespace, runID).Scan(&workerID, &token, &expiresRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, time.Time{}, "", false, nil
	}
	if err != nil {
		return "", 0, time.Time{}, "", false, err
	}
	if t, perr := time.Parse(time.RFC3339Nano, expiresRaw); perr == nil {
		expires = t
	}
	return workerID, token, expires, expiresRaw, true, nil
}

// Claim implements run.Dispatcher.
func (d *LeaseDispatcher) Claim(ctx context.Context, scope execution.NamespaceScope, runID string, worker run.WorkerIdentity, lease time.Duration) (*run.Claim, error) {
	if !scope.HasNamespace() || runID == "" || worker.WorkerID == "" || lease <= 0 {
		return nil, run.ErrInvalidClaim
	}
	now := d.clock()
	expires := now.Add(lease)

	d.mu.Lock()
	defer d.mu.Unlock()

	res, err := d.db.ExecContext(ctx, `
		INSERT INTO dispatch_leases (run_id, namespace, worker_id, fencing_token, lease_expires_at)
		VALUES (?,?,?,1,?)
		ON CONFLICT(namespace, run_id) DO NOTHING`,
		runID, scope.Namespace, worker.WorkerID, expires.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 1 {
		return &run.Claim{RunID: runID, Namespace: scope.Namespace, WorkerID: worker.WorkerID, FencingToken: 1, LeaseExpires: expires}, nil
	}

	owner, token, curExpires, curExpiresRaw, found, err := d.load(ctx, scope.Namespace, runID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, run.ErrLeaseHeld
	}
	if now.Before(curExpires) {
		if owner != worker.WorkerID {
			return nil, run.ErrLeaseHeld
		}
		res, err := d.db.ExecContext(ctx, `
			UPDATE dispatch_leases SET lease_expires_at = ?
			WHERE namespace = ? AND run_id = ? AND worker_id = ? AND fencing_token = ?
			  AND lease_expires_at = ?`,
			expires.UTC().Format(time.RFC3339Nano), scope.Namespace, runID, owner, token,
			curExpiresRaw)
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n == 0 {
			return nil, run.ErrLeaseHeld
		}
		return &run.Claim{RunID: runID, Namespace: scope.Namespace, WorkerID: owner, FencingToken: uint64(token), LeaseExpires: expires}, nil
	}
	// Expired or released: take over with a strictly greater fencing token.
	newToken := token + 1
	res, err = d.db.ExecContext(ctx, `
		UPDATE dispatch_leases SET worker_id = ?, fencing_token = ?, lease_expires_at = ?
		WHERE namespace = ? AND run_id = ? AND worker_id = ? AND fencing_token = ?
		  AND lease_expires_at = ?`,
		worker.WorkerID, newToken, expires.UTC().Format(time.RFC3339Nano), scope.Namespace, runID,
		owner, token, curExpiresRaw)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, run.ErrLeaseHeld
	}
	return &run.Claim{RunID: runID, Namespace: scope.Namespace, WorkerID: worker.WorkerID, FencingToken: uint64(newToken), LeaseExpires: expires}, nil
}

// Heartbeat implements run.Dispatcher.
func (d *LeaseDispatcher) Heartbeat(ctx context.Context, scope execution.NamespaceScope, claim *run.Claim, lease time.Duration) error {
	if !validLeaseClaim(scope, claim) || lease <= 0 {
		return run.ErrInvalidClaim
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	owner, token, expires, expiresRaw, found, err := d.load(ctx, scope.Namespace, claim.RunID)
	if err != nil {
		return err
	}
	if !found || owner != claim.WorkerID || uint64(token) != claim.FencingToken || !now.Before(expires) {
		return run.ErrStaleFencingToken
	}
	res, err := d.db.ExecContext(ctx, `
		UPDATE dispatch_leases SET lease_expires_at = ?
		WHERE namespace = ? AND run_id = ? AND worker_id = ? AND fencing_token = ?
		  AND lease_expires_at = ?`,
		now.Add(lease).UTC().Format(time.RFC3339Nano), scope.Namespace, claim.RunID,
		claim.WorkerID, int64(claim.FencingToken), expiresRaw)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return run.ErrStaleFencingToken
	}
	return nil
}

// Release implements run.Dispatcher.
func (d *LeaseDispatcher) Release(ctx context.Context, scope execution.NamespaceScope, claim *run.Claim) error {
	if !validLeaseClaim(scope, claim) {
		return run.ErrInvalidClaim
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	owner, token, expires, expiresRaw, found, err := d.load(ctx, scope.Namespace, claim.RunID)
	if err != nil {
		return err
	}
	if !found || owner != claim.WorkerID || uint64(token) != claim.FencingToken || !now.Before(expires) {
		return run.ErrStaleFencingToken
	}
	res, err := d.db.ExecContext(ctx, `
		UPDATE dispatch_leases SET worker_id = '', lease_expires_at = ?
		WHERE namespace = ? AND run_id = ? AND worker_id = ? AND fencing_token = ?
		  AND lease_expires_at = ?`,
		time.Unix(0, 0).UTC().Format(time.RFC3339Nano), scope.Namespace, claim.RunID,
		claim.WorkerID, int64(claim.FencingToken), expiresRaw)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return run.ErrStaleFencingToken
	}
	return nil
}

func validLeaseClaim(scope execution.NamespaceScope, claim *run.Claim) bool {
	return scope.HasNamespace() && claim != nil && claim.Namespace == scope.Namespace &&
		claim.RunID != "" && claim.WorkerID != "" && claim.FencingToken != 0
}
