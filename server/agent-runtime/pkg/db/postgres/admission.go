package postgres

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
)

// ClusterAdmissionConfig limits concurrent runs across every replica that
// shares the database. Zero limits are unlimited.
type ClusterAdmissionConfig struct {
	Total     int
	PerTenant int
	// LeaseTTL is how long a slot survives without renewal; holders renew
	// every third of it. Default 30s.
	LeaseTTL time.Duration
	// Holder labels slots for diagnostics (e.g. the replica name).
	Holder string
}

// ClusterAdmission returns a run.ClusterAdmission backed by leased rows in
// agent_runtime_admission_slots. Claims are serialized with a transaction
// advisory lock; slots of crashed replicas expire after LeaseTTL.
func (d *DB) ClusterAdmission(cfg ClusterAdmissionConfig) run.ClusterAdmission {
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	return &clusterAdmission{db: d, cfg: cfg}
}

type clusterAdmission struct {
	db  *DB
	cfg ClusterAdmissionConfig
}

// admissionLockKey is an arbitrary constant naming the claim lock.
const admissionLockKey = 0x78696e67 // "xing"

func (a *clusterAdmission) TryAcquire(ctx context.Context, tenant string) (func(), bool, error) {
	tx, err := a.db.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, admissionLockKey); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_runtime_admission_slots WHERE expires_at < now()`); err != nil {
		return nil, false, err
	}
	var total, perTenant int
	if err := tx.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE tenant = $1) FROM agent_runtime_admission_slots`, tenant).Scan(&total, &perTenant); err != nil {
		return nil, false, err
	}
	if (a.cfg.Total > 0 && total >= a.cfg.Total) || (tenant != "" && a.cfg.PerTenant > 0 && perTenant >= a.cfg.PerTenant) {
		return nil, false, tx.Commit()
	}
	slotID := uuid.NewString()
	ttl := a.cfg.LeaseTTL.Seconds()
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_runtime_admission_slots (slot_id, tenant, holder, expires_at)
		VALUES ($1, $2, $3, now() + $4 * interval '1 second')`, slotID, tenant, a.cfg.Holder, ttl); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(a.cfg.LeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(context.Background(), a.cfg.LeaseTTL/3)
				a.renew(renewCtx, slotID, tenant, ttl)
				cancel()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			// A renewal in flight must not re-insert the slot after the delete.
			<-finished
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// A failed delete only delays capacity until the lease expires.
			_, _ = a.db.db.ExecContext(releaseCtx, `DELETE FROM agent_runtime_admission_slots WHERE slot_id = $1`, slotID)
		})
	}, true, nil
}

// renew extends a slot's lease. A slot that expired while renewals failed
// (and was swept by another claim) is re-inserted, since its run is still
// executing; capacity may briefly overshoot but is never undercounted.
func (a *clusterAdmission) renew(ctx context.Context, slotID, tenant string, ttl float64) {
	result, err := a.db.db.ExecContext(ctx, `UPDATE agent_runtime_admission_slots
		SET expires_at = now() + $2 * interval '1 second' WHERE slot_id = $1`, slotID, ttl)
	if err != nil {
		return
	}
	if n, err := result.RowsAffected(); err != nil || n > 0 {
		return
	}
	_, _ = a.db.db.ExecContext(ctx, `INSERT INTO agent_runtime_admission_slots (slot_id, tenant, holder, expires_at)
		VALUES ($1, $2, $3, now() + $4 * interval '1 second') ON CONFLICT (slot_id) DO NOTHING`, slotID, tenant, a.cfg.Holder, ttl)
}
