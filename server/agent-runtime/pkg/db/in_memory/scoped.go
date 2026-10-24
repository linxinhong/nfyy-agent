package in_memory

import (
	"context"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

var _ db.ScopedAgentSessionStore = (*DB)(nil)
var _ db.ScopedRunStore = (*DB)(nil)

func validScope(ctx context.Context, namespace string) error {
	if err := execution.ValidateNamespace(namespace); err != nil {
		return err
	}
	if err := execution.ValidateNamespaceLength(namespace); err != nil {
		return err
	}
	resolved, err := db.NamespaceFor(ctx, namespace)
	if err != nil {
		return err
	}
	if resolved != namespace {
		return db.ErrNotFound
	}
	return nil
}

func (d *DB) CreateSessionScoped(ctx context.Context, s *session.AgentSession) error {
	if err := validScope(ctx, s.Namespace); err != nil {
		return err
	}
	return d.CreateSession(context.Background(), s)
}

func (d *DB) ReadSessionScoped(ctx context.Context, namespace, sessionID string) (*session.AgentSession, error) {
	if err := validScope(ctx, namespace); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.sessions[nsKey(namespace, sessionID)]
	if !ok {
		return nil, nil
	}
	return cloneSession(s)
}

func (d *DB) UpdateSessionFieldsScoped(ctx context.Context, u db.SessionFieldsUpdate) (uint64, error) {
	if err := validScope(ctx, u.Namespace); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[nsKey(u.Namespace, u.SessionID)]
	if !ok {
		return 0, db.ErrNotFound
	}
	if s.Revision != u.ExpectedRevision || u.ExpectedRevision == 0 {
		return 0, db.ErrConflict
	}
	clone, err := cloneSession(s)
	if err != nil {
		return 0, err
	}
	applyFields(&clone.Metadata, &clone.SessionState, &clone.Summary, u)
	clone.Revision++
	clone.UpdatedAt = time.Now().UTC()
	d.sessions[nsKey(u.Namespace, u.SessionID)] = clone
	return clone.Revision, nil
}

func (d *DB) DeleteSessionScoped(ctx context.Context, namespace, sessionID string) error {
	if err := validScope(ctx, namespace); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := nsKey(namespace, sessionID)
	if _, ok := d.sessions[key]; !ok {
		return db.ErrNotFound
	}
	delete(d.sessions, key)
	set := d.sessionIndex[sessionID]
	delete(set, nsPart(namespace))
	if len(set) == 0 {
		delete(d.sessionIndex, sessionID)
	}
	return nil
}

func (d *DB) ListSessionsScoped(ctx context.Context, q db.SessionQuery) ([]*session.AgentSession, error) {
	if err := validScope(ctx, q.Namespace); err != nil {
		return nil, err
	}
	return d.ListSessions(context.Background(), q)
}

func (d *DB) GetRunScoped(ctx context.Context, q db.RunQuery) (*session.RunOutput, error) {
	if err := validScope(ctx, q.Namespace); err != nil {
		return nil, err
	}
	return d.GetRun(context.Background(), q)
}
