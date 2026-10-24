package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

// LeaseDispatcher is the PostgreSQL multi-instance implementation of
// run.Dispatcher. Claims, takeovers, and fencing-token increments are
// enforced by atomic conditional SQL so concurrent workers agree on a single
// owner without an external lock.
type LeaseDispatcher struct {
	db    *sql.DB
	store *DB
	clock run.Clock
}

// NewLeaseDispatcher wraps an existing PostgreSQL DB for lease management.
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

// Claim implements run.Dispatcher.
func (d *LeaseDispatcher) Claim(ctx context.Context, scope execution.NamespaceScope, runID string, worker run.WorkerIdentity, lease time.Duration) (*run.Claim, error) {
	if !scope.HasNamespace() || runID == "" || worker.WorkerID == "" || lease <= 0 {
		return nil, run.ErrInvalidClaim
	}
	now := d.clock()
	expires := now.Add(lease)

	// 1. Fresh insert owns the run with fencing token 1. On conflict the
	// row already exists and we fall through to takeover/ownership checks.
	res, err := d.db.ExecContext(ctx, `
		INSERT INTO agent_runtime_dispatch_leases (run_id, namespace, worker_id, fencing_token, lease_expires_at)
		VALUES ($1,$2,$3,1,$4)
		ON CONFLICT (namespace, run_id) DO NOTHING`,
		runID, scope.Namespace, worker.WorkerID, expires)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return &run.Claim{RunID: runID, Namespace: scope.Namespace, WorkerID: worker.WorkerID, FencingToken: 1, LeaseExpires: expires}, nil
	}

	// 2. Take over an expired lease atomically: only one worker can flip a
	// live expiry to a future one, and the fencing token strictly increases.
	var token int64
	err = d.db.QueryRowContext(ctx, `
		UPDATE agent_runtime_dispatch_leases
		SET worker_id=$1, fencing_token=fencing_token+1, lease_expires_at=$2
		WHERE namespace=$3 AND run_id=$4 AND lease_expires_at <= $5
		RETURNING fencing_token`,
		worker.WorkerID, expires, scope.Namespace, runID, now).Scan(&token)
	if err == nil {
		return &run.Claim{RunID: runID, Namespace: scope.Namespace, WorkerID: worker.WorkerID, FencingToken: uint64(token), LeaseExpires: expires}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// 3. Refresh a live lease for the same worker in one conditional write.
	// The ownership and expiry predicates prevent a read-then-update race
	// from extending a lease that another worker has already taken over.
	err = d.db.QueryRowContext(ctx, `
		UPDATE agent_runtime_dispatch_leases SET lease_expires_at=$1
		WHERE namespace=$2 AND run_id=$3 AND worker_id=$4 AND lease_expires_at > $5
		RETURNING fencing_token`,
		expires, scope.Namespace, runID, worker.WorkerID, now).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, run.ErrLeaseHeld
	}
	if err != nil {
		return nil, err
	}
	return &run.Claim{RunID: runID, Namespace: scope.Namespace, WorkerID: worker.WorkerID, FencingToken: uint64(token), LeaseExpires: expires}, nil
}

// Heartbeat implements run.Dispatcher.
func (d *LeaseDispatcher) Heartbeat(ctx context.Context, scope execution.NamespaceScope, claim *run.Claim, lease time.Duration) error {
	if !validLeaseClaim(scope, claim) || lease <= 0 {
		return run.ErrInvalidClaim
	}
	now := d.clock()
	res, err := d.db.ExecContext(ctx, `
		UPDATE agent_runtime_dispatch_leases SET lease_expires_at=$1
		WHERE namespace=$2 AND run_id=$3 AND worker_id=$4 AND fencing_token=$5
		  AND lease_expires_at > $6`,
		now.Add(lease), scope.Namespace, claim.RunID, claim.WorkerID, int64(claim.FencingToken), now)
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
	now := d.clock()
	res, err := d.db.ExecContext(ctx, `
		UPDATE agent_runtime_dispatch_leases SET worker_id='', lease_expires_at=$1
		WHERE namespace=$2 AND run_id=$3 AND worker_id=$4 AND fencing_token=$5
		  AND lease_expires_at > $6`,
		time.Unix(0, 0).UTC(), scope.Namespace, claim.RunID, claim.WorkerID,
		int64(claim.FencingToken), now)
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

// GetRun, AppendRun, and UpdateRun preserve the existing non-managed path.
func (d *LeaseDispatcher) GetRun(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	return d.store.GetRun(ctx, query)
}

func (d *LeaseDispatcher) AppendRun(ctx context.Context, output *session.RunOutput) error {
	return d.store.AppendRun(ctx, output)
}

func (d *LeaseDispatcher) UpdateRun(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	return d.store.UpdateRun(ctx, output, expected)
}

// GetManagedRun reads a namespace-scoped Run and its independent content
// revision. The lease generation is intentionally not used as this revision.
func (d *LeaseDispatcher) GetManagedRun(ctx context.Context, scope execution.NamespaceScope, runID string) (*run.ManagedRunSnapshot, error) {
	if !scope.HasNamespace() || runID == "" {
		return nil, run.ErrInvalidClaim
	}
	var raw []byte
	var revision uint64
	err := d.db.QueryRowContext(ctx, `SELECT payload,revision FROM agent_runtime_runs WHERE namespace=$1 AND run_id=$2`, scope.Namespace, runID).Scan(&raw, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, db.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var output session.RunOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		return nil, err
	}
	if output.WorkflowID != "" && output.Workflow == nil {
		output.Workflow = &workflowschema.RunData{Legacy: true}
	}
	return &run.ManagedRunSnapshot{Run: &output, Revision: revision}, nil
}

// CommitManagedRun locks and validates the authoritative lease in the same
// database transaction that performs the Run revision/status CAS.
func (d *LeaseDispatcher) CommitManagedRun(ctx context.Context, scope execution.NamespaceScope, claim *run.Claim, commit run.ManagedRunCommit) (uint64, error) {
	if !validLeaseClaim(scope, claim) || commit.Run == nil || commit.Run.RunID == "" ||
		commit.Run.RunID != claim.RunID || commit.Run.Namespace != scope.Namespace {
		return 0, run.ErrInvalidClaim
	}
	kind, componentID, err := runParent(commit.Run)
	if err != nil {
		return 0, err
	}
	if commit.Run.SessionID == "" {
		return 0, fmt.Errorf("session_id is empty")
	}
	if commit.Run.CreatedAt.IsZero() {
		commit.Run.CreatedAt = d.clock().UTC()
	}
	if commit.Run.UpdatedAt.IsZero() {
		commit.Run.UpdatedAt = commit.Run.CreatedAt
	}
	raw, err := json.Marshal(commit.Run)
	if err != nil {
		return 0, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var owner string
	var generation uint64
	var expires time.Time
	err = tx.QueryRowContext(ctx, `SELECT worker_id,fencing_token,lease_expires_at
		FROM agent_runtime_dispatch_leases WHERE namespace=$1 AND run_id=$2 FOR UPDATE`,
		scope.Namespace, claim.RunID).Scan(&owner, &generation, &expires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (owner != claim.WorkerID || generation != claim.FencingToken) {
		return 0, run.ErrStaleFencingToken
	}
	if err != nil {
		return 0, err
	}
	if !d.clock().Before(expires) {
		return 0, run.ErrLeaseExpired
	}

	var parentExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runtime_sessions
		WHERE namespace=$1 AND session_id=$2 AND kind=$3 AND component_id=$4 AND user_id=$5)`,
		scope.Namespace, commit.Run.SessionID, kind, componentID, commit.Run.UserID).Scan(&parentExists); err != nil {
		return 0, err
	}
	if !parentExists {
		return 0, db.ErrNotFound
	}

	var revision uint64
	if commit.ExpectedRevision == 0 && commit.ExpectedStatus == "" {
		err = tx.QueryRowContext(ctx, `INSERT INTO agent_runtime_runs
			(run_id,session_id,kind,user_id,payload,revision,created_at,updated_at,namespace)
			VALUES($1,$2,$3,$4,$5::jsonb,1,$6,$7,$8)
			ON CONFLICT (run_id) DO NOTHING RETURNING revision`,
			commit.Run.RunID, commit.Run.SessionID, kind, commit.Run.UserID, string(raw),
			commit.Run.CreatedAt, commit.Run.UpdatedAt, scope.Namespace).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, db.ErrConflict
		}
	} else {
		if commit.ExpectedRevision == 0 || !commit.ExpectedStatus.CanTransitionTo(commit.Run.Status) {
			if commit.ExpectedRevision == 0 {
				return 0, db.ErrConflict
			}
			return 0, db.ErrInvalidTransition
		}
		err = tx.QueryRowContext(ctx, `UPDATE agent_runtime_runs
			SET payload=$1::jsonb,revision=revision+1,updated_at=$2
			WHERE namespace=$3 AND run_id=$4 AND session_id=$5 AND kind=$6 AND user_id=$7
			  AND revision=$8 AND payload->>'status'=$9 RETURNING revision`,
			string(raw), commit.Run.UpdatedAt, scope.Namespace, commit.Run.RunID, commit.Run.SessionID,
			kind, commit.Run.UserID, commit.ExpectedRevision, string(commit.ExpectedStatus)).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			var exists bool
			if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runtime_runs WHERE namespace=$1 AND run_id=$2)`, scope.Namespace, commit.Run.RunID).Scan(&exists); e != nil {
				return 0, e
			}
			if !exists {
				return 0, db.ErrNotFound
			}
			return 0, db.ErrConflict
		}
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_sessions SET updated_at=GREATEST(updated_at,$1)
		WHERE namespace=$2 AND session_id=$3 AND kind=$4`, commit.Run.UpdatedAt, scope.Namespace, commit.Run.SessionID, kind); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return revision, nil
}
