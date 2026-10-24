package postgres

import (
	"context"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

var _ db.ScopedAgentSessionStore = (*DB)(nil)
var _ db.ScopedRunStore = (*DB)(nil)
var _ db.NamespaceScopedStore = (*DB)(nil)

func (*DB) SupportsNamespaceScopes() {}

func scopedContext(ctx context.Context, namespace string) (context.Context, error) {
	if err := execution.ValidateNamespace(namespace); err != nil {
		return nil, err
	}
	if err := execution.ValidateNamespaceLength(namespace); err != nil {
		return nil, err
	}
	resolved, err := db.NamespaceFor(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if resolved != namespace {
		return nil, db.ErrNotFound
	}
	return execution.ContextWithNamespace(ctx, namespace), nil
}

func (d *DB) CreateSessionScoped(ctx context.Context, s *session.AgentSession) error {
	ctx, err := scopedContext(ctx, s.Namespace)
	if err != nil {
		return err
	}
	return d.CreateSession(ctx, s)
}

func (d *DB) ReadSessionScoped(ctx context.Context, namespace, sessionID string) (*session.AgentSession, error) {
	ctx, err := scopedContext(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return d.ReadSession(ctx, sessionID)
}

func (d *DB) UpdateSessionFieldsScoped(ctx context.Context, u db.SessionFieldsUpdate) (uint64, error) {
	ctx, err := scopedContext(ctx, u.Namespace)
	if err != nil {
		return 0, err
	}
	return d.UpdateSessionFields(ctx, u)
}

func (d *DB) DeleteSessionScoped(ctx context.Context, namespace, sessionID string) error {
	ctx, err := scopedContext(ctx, namespace)
	if err != nil {
		return err
	}
	return d.DeleteSession(ctx, sessionID)
}

func (d *DB) ListSessionsScoped(ctx context.Context, q db.SessionQuery) ([]*session.AgentSession, error) {
	ctx, err := scopedContext(ctx, q.Namespace)
	if err != nil {
		return nil, err
	}
	return d.ListSessions(ctx, q)
}

func (d *DB) GetRunScoped(ctx context.Context, q db.RunQuery) (*session.RunOutput, error) {
	ctx, err := scopedContext(ctx, q.Namespace)
	if err != nil {
		return nil, err
	}
	return d.GetRun(ctx, q)
}
