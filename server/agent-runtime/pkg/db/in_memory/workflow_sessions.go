// Package in_memory provides an in-memory implementation of the db.DB interface.
package in_memory

import (
	"context"
	"fmt"
	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"time"
)

func (db *DB) CreateWorkflowSession(_ context.Context, s *session.WorkflowSession) error {
	if s == nil {
		return fmt.Errorf("session is nil")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if s.SessionID == "" {
		s.SessionID = generateID()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.CreatedAt
	}
	if s.Revision == 0 {
		s.Revision = 1
	}

	clone, err := cloneWorkflowSession(s)
	if err != nil {
		return err
	}
	key := nsKey(s.Namespace, s.SessionID)
	db.workflowSessions[key] = clone
	indexAdd(db.workflowSessionIndex, s.Namespace, s.SessionID)
	return nil
}

// ReadWorkflowSession returns a workflow session by id. Returns nil, nil if not found.
func (db *DB) ReadWorkflowSession(_ context.Context, sessionID string) (*session.WorkflowSession, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is empty")
	}

	db.mu.RLock()
	defer db.mu.RUnlock()

	key, ok, err := findKey(db.workflowSessionIndex, sessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	s, ok := db.workflowSessions[key]
	if !ok {
		return nil, nil
	}
	return cloneWorkflowSession(s)
}

// UpdateWorkflowSession replaces an existing workflow session.
func (db *DB) UpdateWorkflowSession(_ context.Context, s *session.WorkflowSession) error {
	if s == nil {
		return fmt.Errorf("session is nil")
	}
	if s.SessionID == "" {
		return fmt.Errorf("session_id is empty")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	existing, ok := db.workflowSessions[nsKey(s.Namespace, s.SessionID)]
	if !ok {
		return dbpkg.ErrNotFound
	}
	if s.Revision == 0 || s.Revision != existing.Revision {
		return dbpkg.ErrConflict
	}

	clone, err := cloneWorkflowSession(s)
	if err != nil {
		return err
	}
	if clone.UpdatedAt.IsZero() {
		clone.UpdatedAt = time.Now().UTC()
	}
	clone.Runs = existing.Runs
	clone.Revision = existing.Revision + 1
	s.Revision = clone.Revision
	key := nsKey(s.Namespace, s.SessionID)
	db.workflowSessions[key] = clone
	indexAdd(db.workflowSessionIndex, s.Namespace, s.SessionID)
	return nil
}

// DeleteWorkflowSession removes a workflow session by id.
func (db *DB) DeleteWorkflowSession(_ context.Context, sessionID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	key, ok, err := findKey(db.workflowSessionIndex, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return dbpkg.ErrNotFound
	}
	delete(db.workflowSessions, key)
	delete(db.workflowSessionIndex, sessionID)
	return nil
}

// ListWorkflowSessions returns sessions filtered before pagination.
func (db *DB) ListWorkflowSessions(_ context.Context, query dbpkg.SessionQuery) ([]*session.WorkflowSession, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var result []*session.WorkflowSession
	for _, s := range db.workflowSessions {
		if query.Namespace != "" && s.Namespace != query.Namespace {
			continue
		}
		if query.ComponentID != "" && s.WorkflowID != query.ComponentID {
			continue
		}
		if query.UserID != "" && s.UserID != query.UserID {
			continue
		}
		result = append(result, s)
	}

	for i := 0; i < len(result)-1; i++ {
		for j := i + 1; j < len(result); j++ {
			if result[i].UpdatedAt.Before(result[j].UpdatedAt) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Offset > len(result) {
		return []*session.WorkflowSession{}, nil
	}
	result = result[query.Offset:]

	if query.Limit > 0 && query.Limit < len(result) {
		result = result[:query.Limit]
	}

	return cloneWorkflowSessions(result)
}
