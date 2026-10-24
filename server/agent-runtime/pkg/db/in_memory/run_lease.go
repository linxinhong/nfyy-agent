package in_memory

import (
	"context"
	"fmt"
	"time"

	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

var (
	_ dbpkg.RecentSessionReader = (*DB)(nil)
	_ dbpkg.RunLeaseStore       = (*DB)(nil)
)

// ReadSessionRecent returns a session clone with only its newest maxRuns runs.
func (db *DB) ReadSessionRecent(ctx context.Context, sessionID string, maxRuns int) (*session.AgentSession, error) {
	if maxRuns <= 0 {
		return db.ReadSession(ctx, sessionID)
	}
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is empty")
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	key, ok, err := findKey(db.sessionIndex, sessionID)
	if err != nil || !ok {
		return nil, err
	}
	s, ok := db.sessions[key]
	if !ok {
		return nil, nil
	}
	trimmed := *s
	if len(trimmed.Runs) > maxRuns {
		trimmed.Runs = trimmed.Runs[len(trimmed.Runs)-maxRuns:]
	}
	return cloneSession(&trimmed)
}

// allRuns returns the run slices of every session kind visible to query.
// The caller holds db.mu.
func (db *DB) allRuns(query dbpkg.RunQuery) [][]*session.RunOutput {
	var out [][]*session.RunOutput
	match := func(namespace, userID string) bool {
		return (query.Namespace == "" || namespace == query.Namespace) && (query.UserID == "" || userID == query.UserID)
	}
	for _, s := range db.sessions {
		if match(s.Namespace, s.UserID) {
			out = append(out, s.Runs)
		}
	}
	for _, s := range db.teamSessions {
		if match(s.Namespace, s.UserID) {
			out = append(out, s.Runs)
		}
	}
	for _, s := range db.workflowSessions {
		if match(s.Namespace, s.UserID) {
			out = append(out, s.Runs)
		}
	}
	return out
}

// TouchRun refreshes the heartbeat of a running run of any session kind.
func (db *DB) TouchRun(_ context.Context, query dbpkg.RunQuery) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, runs := range db.allRuns(query) {
		for _, run := range runs {
			if run.RunID != query.RunID {
				continue
			}
			if run.Status != session.RunStatusRunning {
				return dbpkg.ErrConflict
			}
			run.UpdatedAt = time.Now().UTC()
			return nil
		}
	}
	return dbpkg.ErrNotFound
}

// ExpireStaleRuns fails leased running runs whose heartbeat expired.
func (db *DB) ExpireStaleRuns(_ context.Context, now time.Time, reason string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	var ids []string
	for _, runs := range db.allRuns(dbpkg.RunQuery{}) {
		for _, run := range runs {
			if len(ids) >= limit {
				return ids, nil
			}
			if run.Status != session.RunStatusRunning || run.LeaseTimeout <= 0 || !run.UpdatedAt.Before(now.Add(-run.LeaseTimeout)) {
				continue
			}
			run.Status = session.RunStatusError
			run.Error = reason
			run.UpdatedAt = now.UTC()
			ids = append(ids, run.RunID)
		}
	}
	return ids, nil
}
