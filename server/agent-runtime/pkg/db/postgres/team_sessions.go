// Package postgres provides a PostgreSQL-backed implementation of the db.DB interface.
package postgres

import (
	"context"
	"encoding/json"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"time"
)

func (d *DB) CreateTeamSession(ctx context.Context, s *session.TeamSession) error {
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
	if e = createSession(ctx, tx, sessionRecord{ID: s.SessionID, Kind: "team", ComponentID: s.TeamID, UserID: s.UserID, Namespace: namespace, Payload: raw, Revision: s.Revision, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}); e != nil {
		return e
	}
	for _, r := range runs {
		if r.SessionID == "" {
			r.SessionID = s.SessionID
		}
		if r.TeamID == "" {
			r.TeamID = s.TeamID
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
func (d *DB) ReadTeamSession(ctx context.Context, id string) (*session.TeamSession, error) {
	r, e := d.readRecord(ctx, "team", id)
	if e != nil || r == nil {
		return nil, e
	}
	var s session.TeamSession
	if e = json.Unmarshal(r.Payload, &s); e != nil {
		return nil, e
	}
	s.Namespace = r.Namespace
	s.Revision = r.Revision
	s.CreatedAt, s.UpdatedAt = r.CreatedAt, r.UpdatedAt
	s.Runs, e = d.loadRuns(ctx, id)
	return &s, e
}
func (d *DB) UpdateTeamSession(ctx context.Context, s *session.TeamSession) error {
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
	rev, e := d.updateRecord(ctx, sessionRecord{ID: s.SessionID, Kind: "team", ComponentID: s.TeamID, UserID: s.UserID, Namespace: s.Namespace, Payload: raw, Revision: s.Revision, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt})
	if e == nil {
		s.Revision = rev
	}
	return e
}
func (d *DB) DeleteTeamSession(ctx context.Context, id string) error {
	return d.deleteSession(ctx, "team", id)
}
func (d *DB) ListTeamSessions(ctx context.Context, q db.SessionQuery) ([]*session.TeamSession, error) {
	rs, e := d.listRecords(ctx, "team", q)
	if e != nil {
		return nil, e
	}
	out := make([]*session.TeamSession, 0, len(rs))
	for _, r := range rs {
		var s session.TeamSession
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
		out = append(out, &s)
	}
	return out, nil
}
