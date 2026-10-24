// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// GetSessionState returns the current session state for the given session id.
// If the agent has no DB configured, it returns an error.
func (a *Agent) GetSessionState(ctx context.Context, sessionID string) (map[string]any, error) {
	if a.cfg.DB == nil {
		return nil, errors.New("agent: no DB configured")
	}
	sess, err := a.cfg.DB.ReadSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	return sess.SessionState, nil
}

// UpdateSessionState merges updates into the session's state and persists it.
// The read-modify-write is retried on revision conflicts so concurrent
// updates of different keys are not lost.
func (a *Agent) UpdateSessionState(ctx context.Context, sessionID string, updates map[string]any) error {
	if a.cfg.DB == nil {
		return errors.New("agent: no DB configured")
	}
	const attempts = 5
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		var sess *session.AgentSession
		sess, err = a.cfg.DB.ReadSession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("read session: %w", err)
		}
		if sess == nil {
			return fmt.Errorf("session not found: %s", sessionID)
		}
		if sess.SessionState == nil {
			sess.SessionState = make(map[string]any)
		}
		for k, v := range updates {
			sess.SessionState[k] = v
		}
		_, err = a.cfg.DB.UpdateSessionFields(ctx, db.SessionFieldsUpdate{SessionID: sessionID, ExpectedRevision: sess.Revision, SessionState: sess.SessionState})
		if !errors.Is(err, db.ErrConflict) {
			return err
		}
	}
	return err
}

// GetCurrentSessionState returns the state of the session currently bound to
// the agent via SessionID, or nil if none is bound.
func (a *Agent) GetCurrentSessionState(ctx context.Context) (map[string]any, error) {
	_, sessionID := a.defaultIdentity()
	if sessionID == "" {
		return nil, nil
	}
	return a.GetSessionState(ctx, sessionID)
}

// UpdateCurrentSessionState merges updates into the currently bound session.
func (a *Agent) UpdateCurrentSessionState(ctx context.Context, updates map[string]any) error {
	_, sessionID := a.defaultIdentity()
	if sessionID == "" {
		return errors.New("agent: no SessionID configured")
	}
	return a.UpdateSessionState(ctx, sessionID, updates)
}

// loadOrCreateSession now seeds a new session with any configured SessionState.
func (a *Agent) loadOrCreateSessionWithState(ctx context.Context) (*session.AgentSession, error) {
	sess, err := a.loadOrCreateSession(ctx)
	if err != nil {
		return nil, err
	}
	if sess.SessionState == nil && len(a.cfg.SessionState) > 0 {
		sess.SessionState = make(map[string]any, len(a.cfg.SessionState))
		for k, v := range a.cfg.SessionState {
			sess.SessionState[k] = v
		}
		if err := a.saveSession(ctx, sess); err != nil {
			return nil, fmt.Errorf("save initial session state: %w", err)
		}
	}
	return sess, nil
}
