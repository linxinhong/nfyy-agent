package postgres

import (
	"context"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
)

var _ db.SessionRetentionStore = (*DB)(nil)

// DeleteIdleSessions deletes idle sessions; their runs go with them through
// ON DELETE CASCADE. SKIP LOCKED lets several replicas clean concurrently.
func (d *DB) DeleteIdleSessions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	result, err := d.db.ExecContext(ctx, `DELETE FROM agent_runtime_sessions WHERE session_id IN (
		SELECT s.session_id FROM agent_runtime_sessions s
		WHERE s.updated_at < $1 AND NOT EXISTS (
			SELECT 1 FROM agent_runtime_runs r
			WHERE r.session_id = s.session_id AND r.payload->>'status' IN ('pending','running','paused'))
		ORDER BY s.updated_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}
