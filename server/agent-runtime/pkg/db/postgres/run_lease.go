package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

var (
	_ db.RecentSessionReader = (*DB)(nil)
	_ db.RunLeaseStore       = (*DB)(nil)
)

// ReadSessionRecent reads an agent session with only its newest maxRuns runs.
// The (session_id, created_at) index serves the bounded descending scan.
func (d *DB) ReadSessionRecent(ctx context.Context, id string, maxRuns int) (*session.AgentSession, error) {
	if maxRuns <= 0 {
		return d.ReadSession(ctx, id)
	}
	r, err := d.readRecord(ctx, "agent", id)
	if err != nil || r == nil {
		return nil, err
	}
	var s session.AgentSession
	if err := json.Unmarshal(r.Payload, &s); err != nil {
		return nil, err
	}
	s.Namespace = r.Namespace
	s.Revision = r.Revision
	s.CreatedAt, s.UpdatedAt = r.CreatedAt, r.UpdatedAt
	rows, err := d.db.QueryContext(ctx, `SELECT payload FROM (
		SELECT payload, created_at FROM agent_runtime_runs WHERE namespace=$1 AND session_id=$2
		ORDER BY created_at DESC LIMIT $3) recent ORDER BY created_at`, r.Namespace, id, maxRuns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var run session.RunOutput
		if err := json.Unmarshal(raw, &run); err != nil {
			return nil, err
		}
		run.Namespace = r.Namespace
		s.Runs = append(s.Runs, &run)
	}
	return &s, rows.Err()
}

// TouchRun refreshes the heartbeat of a running run without rewriting its
// payload, so it is safe to call concurrently with the run's own updates.
// The heartbeat uses the database clock, as ExpireStaleRuns does, so clock
// skew between replicas cannot make a live run look stale.
func (d *DB) TouchRun(ctx context.Context, q db.RunQuery) error {
	namespace, err := db.NamespaceFor(ctx, q.Namespace)
	if err != nil {
		return err
	}
	// The payload copy of updated_at is refreshed too so GetRun reports the
	// heartbeat; jsonb_set touches only that key.
	result, err := d.db.ExecContext(ctx, `UPDATE agent_runtime_runs
		SET updated_at=date_trunc('microseconds', now()),
			payload=jsonb_set(payload, '{updated_at}', to_jsonb(date_trunc('microseconds', now())))
		WHERE namespace=$1 AND run_id=$2 AND ($3='' OR user_id=$3) AND payload->>'status'='running'`,
		namespace, q.RunID, q.UserID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n > 0 {
		return err
	}
	var marker int
	err = d.db.QueryRowContext(ctx, `SELECT 1 FROM agent_runtime_runs WHERE namespace=$1 AND run_id=$2 AND ($3='' OR user_id=$3)`,
		namespace, q.RunID, q.UserID).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	if err != nil {
		return err
	}
	return db.ErrConflict
}

// ExpireStaleRuns fails leased running runs whose heartbeat expired.
// FOR UPDATE SKIP LOCKED lets several replicas reap concurrently. The
// cutoff uses the earlier of now and the database clock, so a replica whose
// clock runs fast never reaps early.
// lease_timeout is a Go duration in nanoseconds.
func (d *DB) ExpireStaleRuns(ctx context.Context, now time.Time, reason string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	now = now.UTC()
	reasonJSON, err := json.Marshal(reason)
	if err != nil {
		return nil, err
	}
	nowJSON, err := json.Marshal(now)
	if err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `UPDATE agent_runtime_runs r SET
			payload = jsonb_set(jsonb_set(jsonb_set(r.payload, '{status}', '"error"'), '{error}', $1::jsonb), '{updated_at}', $2::jsonb),
			updated_at = $3, revision = r.revision + 1
		WHERE r.ctid IN (
			SELECT ctid FROM agent_runtime_runs
			WHERE payload->>'status'='running' AND payload ? 'lease_timeout'
			  AND updated_at < LEAST($3::timestamptz, now()) - ((payload->>'lease_timeout')::bigint / 1000) * interval '1 microsecond'
			ORDER BY updated_at LIMIT $4 FOR UPDATE SKIP LOCKED)
		RETURNING r.run_id`, string(reasonJSON), string(nowJSON), now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
