// Package postgres provides a PostgreSQL-backed implementation of the db.DB interface.
package postgres

import (
	"context"
	"encoding/json"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"time"
)

func (d *DB) CreateWorkflowSession(ctx context.Context, s *session.WorkflowSession) error {
	namespace, e := db.NamespaceFor(ctx, s.Namespace)
	if e != nil {
		return e
	}
	s.Namespace = namespace
	prepareSession(&s.SessionID, &s.Revision, &s.CreatedAt, &s.UpdatedAt)
	runs := s.Runs
	s.Runs = nil
	raw, e := json.Marshal(s)
	s.Runs = runs
	if e != nil {
		return e
	}
	tx, e := d.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = createSession(ctx, tx, sessionRecord{ID: s.SessionID, Kind: "workflow", ComponentID: s.WorkflowID, UserID: s.UserID, Namespace: namespace, Payload: raw, Revision: s.Revision, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}); e != nil {
		return e
	}
	for _, r := range runs {
		if r.SessionID == "" {
			r.SessionID = s.SessionID
		}
		if r.WorkflowID == "" {
			r.WorkflowID = s.WorkflowID
		}
		if r.UserID == "" {
			r.UserID = s.UserID
		}
		if e = appendRun(ctx, tx, r); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (d *DB) ReadWorkflowSession(ctx context.Context, id string) (*session.WorkflowSession, error) {
	r, e := d.readRecord(ctx, "workflow", id)
	if e != nil || r == nil {
		return nil, e
	}
	var s session.WorkflowSession
	if e = json.Unmarshal(r.Payload, &s); e != nil {
		return nil, e
	}
	s.Namespace = r.Namespace
	s.Revision = r.Revision
	s.CreatedAt, s.UpdatedAt = r.CreatedAt, r.UpdatedAt
	s.Runs, e = d.loadRuns(ctx, id)
	markLegacyWorkflowRuns(s.Runs)
	return &s, e
}
func (d *DB) UpdateWorkflowSession(ctx context.Context, s *session.WorkflowSession) error {
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}
	runs := s.Runs
	s.Runs = nil
	raw, e := json.Marshal(s)
	s.Runs = runs
	if e != nil {
		return e
	}
	rev, e := d.updateRecord(ctx, sessionRecord{ID: s.SessionID, Kind: "workflow", ComponentID: s.WorkflowID, UserID: s.UserID, Namespace: s.Namespace, Payload: raw, Revision: s.Revision, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt})
	if e == nil {
		s.Revision = rev
	}
	return e
}
func (d *DB) DeleteWorkflowSession(ctx context.Context, id string) error {
	return d.deleteSession(ctx, "workflow", id)
}
func (d *DB) ListWorkflowSessions(ctx context.Context, q db.SessionQuery) ([]*session.WorkflowSession, error) {
	rs, e := d.listRecords(ctx, "workflow", q)
	if e != nil {
		return nil, e
	}
	out := make([]*session.WorkflowSession, 0, len(rs))
	for _, r := range rs {
		var s session.WorkflowSession
		if e = json.Unmarshal(r.Payload, &s); e != nil {
			return nil, e
		}
		s.Namespace = r.Namespace
		s.Revision = r.Revision
		s.CreatedAt, s.UpdatedAt = r.CreatedAt, r.UpdatedAt
		s.Runs, e = d.loadRuns(ctx, r.ID)
		if e != nil {
			return nil, e
		}
		markLegacyWorkflowRuns(s.Runs)
		out = append(out, &s)
	}
	return out, nil
}
