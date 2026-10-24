package in_memory

import (
	"context"
	"time"

	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

var _ dbpkg.SessionRetentionStore = (*DB)(nil)

func idle(updated time.Time, cutoff time.Time, runs []*session.RunOutput) bool {
	if !updated.Before(cutoff) {
		return false
	}
	for _, run := range runs {
		switch run.Status {
		case session.RunStatusPending, session.RunStatusRunning, session.RunStatusPaused:
			return false
		}
	}
	return true
}

func unindex(index map[string]map[string]struct{}, namespace, id string) {
	if set := index[id]; set != nil {
		delete(set, nsPart(namespace))
		if len(set) == 0 {
			delete(index, id)
		}
	}
}

// DeleteIdleSessions deletes idle agent, team, and workflow sessions.
func (db *DB) DeleteIdleSessions(_ context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	deleted := 0
	for key, s := range db.sessions {
		if deleted < limit && idle(s.UpdatedAt, cutoff, s.Runs) {
			delete(db.sessions, key)
			unindex(db.sessionIndex, s.Namespace, s.SessionID)
			deleted++
		}
	}
	for key, s := range db.teamSessions {
		if deleted < limit && idle(s.UpdatedAt, cutoff, s.Runs) {
			delete(db.teamSessions, key)
			unindex(db.teamSessionIndex, s.Namespace, s.SessionID)
			deleted++
		}
	}
	for key, s := range db.workflowSessions {
		if deleted < limit && idle(s.UpdatedAt, cutoff, s.Runs) {
			delete(db.workflowSessions, key)
			unindex(db.workflowSessionIndex, s.Namespace, s.SessionID)
			deleted++
		}
	}
	return deleted, nil
}
