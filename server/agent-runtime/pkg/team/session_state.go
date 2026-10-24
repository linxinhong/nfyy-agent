// Package team implements multi-agent team orchestration.
package team

import (
	"context"
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// GetSessionState returns a copy of the persisted session state for a team session.
func (t *Team) GetSessionState(ctx context.Context, sessionID string) (map[string]any, error) {
	sess, err := t.loadTeamSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return copyMap(sess.SessionState), nil
}

// UpdateSessionState merges the provided state into the persisted team session state.
func (t *Team) UpdateSessionState(ctx context.Context, sessionID string, state map[string]any) error {
	sess, err := t.loadTeamSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if sess.SessionState == nil {
		sess.SessionState = map[string]any{}
	}
	for k, v := range state {
		sess.SessionState[k] = v
	}
	_, err = t.cfg.DB.UpdateTeamSessionFields(ctx, db.SessionFieldsUpdate{SessionID: sessionID, ExpectedRevision: sess.Revision, SessionState: sess.SessionState})
	return err
}

// loadTeamSession reads a team session from the configured DB.
func (t *Team) loadTeamSession(ctx context.Context, sessionID string) (*session.TeamSession, error) {
	if t.cfg.DB == nil {
		return nil, fmt.Errorf("team: no DB configured")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("team: sessionID is required")
	}
	sess, err := t.cfg.DB.ReadTeamSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read team session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("team session not found: %s", sessionID)
	}
	return sess, nil
}

func copyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
