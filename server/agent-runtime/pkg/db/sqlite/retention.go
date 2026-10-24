package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
)

var _ db.SessionRetentionStore = (*DB)(nil)

// DeleteIdleSessions deletes idle agent, team, and workflow sessions; runs
// and run messages go with them through ON DELETE CASCADE. Timestamps are
// RFC 3339 text, which compares correctly at the granularity retention
// uses (sub-second ordering within one second is not guaranteed).
func (d *DB) DeleteIdleSessions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	cutoffText := cutoff.UTC().Format(time.RFC3339Nano)
	deleted := 0
	for _, kind := range []struct{ sessions, runs string }{
		{"sessions", "runs"}, {"team_sessions", "team_runs"}, {"workflow_sessions", "workflow_runs"},
	} {
		if deleted >= limit {
			break
		}
		result, err := d.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %[1]s WHERE session_id IN (
			SELECT s.session_id FROM %[1]s s WHERE s.updated_at < ? AND NOT EXISTS (
				SELECT 1 FROM %[2]s r WHERE r.session_id = s.session_id AND r.status IN ('pending','running','paused'))
			ORDER BY s.updated_at LIMIT ?)`, kind.sessions, kind.runs), cutoffText, limit-deleted)
		if err != nil {
			return deleted, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += int(n)
	}
	return deleted, nil
}
