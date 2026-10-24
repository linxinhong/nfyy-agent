// Package sqlite provides a SQLite-backed implementation of the db.DB interface.
package sqlite

import (
	"context"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
)

func applyFields(metadata *map[string]any, state *map[string]any, summary *string, update db.SessionFieldsUpdate) {
	if update.Metadata != nil {
		*metadata = update.Metadata
	}
	if update.SessionState != nil {
		*state = update.SessionState
	}
	if update.Summary != nil {
		*summary = *update.Summary
	}
}
func (d *DB) UpdateSessionFields(ctx context.Context, u db.SessionFieldsUpdate) (uint64, error) {
	s, e := d.ReadSession(ctx, u.SessionID)
	if e != nil {
		return 0, e
	}
	if s == nil {
		return 0, db.ErrNotFound
	}
	s.Revision = u.ExpectedRevision
	applyFields(&s.Metadata, &s.SessionState, &s.Summary, u)
	s.UpdatedAt = time.Now().UTC()
	e = d.UpdateSession(ctx, s)
	return s.Revision, e
}
func (d *DB) UpdateTeamSessionFields(ctx context.Context, u db.SessionFieldsUpdate) (uint64, error) {
	s, e := d.ReadTeamSession(ctx, u.SessionID)
	if e != nil {
		return 0, e
	}
	if s == nil {
		return 0, db.ErrNotFound
	}
	s.Revision = u.ExpectedRevision
	applyFields(&s.Metadata, &s.SessionState, &s.Summary, u)
	s.UpdatedAt = time.Now().UTC()
	e = d.UpdateTeamSession(ctx, s)
	return s.Revision, e
}
func (d *DB) UpdateWorkflowSessionFields(ctx context.Context, u db.SessionFieldsUpdate) (uint64, error) {
	s, e := d.ReadWorkflowSession(ctx, u.SessionID)
	if e != nil {
		return 0, e
	}
	if s == nil {
		return 0, db.ErrNotFound
	}
	s.Revision = u.ExpectedRevision
	applyFields(&s.Metadata, &s.SessionState, &s.Summary, u)
	s.UpdatedAt = time.Now().UTC()
	e = d.UpdateWorkflowSession(ctx, s)
	return s.Revision, e
}

// CreateMemory inserts a new memory.
