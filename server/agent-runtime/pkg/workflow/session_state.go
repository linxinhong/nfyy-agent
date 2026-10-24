// Package workflow implements sequential step-based workflow execution.
package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
)

func (w *Workflow) getSessionState(ctx context.Context, sessionID string) (map[string]any, error) {
	if w.cfg.DB == nil {
		return nil, errors.New("workflow: no DB configured")
	}
	sess, err := w.cfg.DB.ReadWorkflowSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read workflow session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("workflow session not found: %s", sessionID)
	}
	return sess.SessionState, nil
}

func (w *Workflow) updateSessionState(ctx context.Context, sessionID string, updates map[string]any) error {
	if w.cfg.DB == nil {
		return errors.New("workflow: no DB configured")
	}
	sess, err := w.cfg.DB.ReadWorkflowSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("read workflow session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("workflow session not found: %s", sessionID)
	}
	if sess.SessionState == nil {
		sess.SessionState = make(map[string]any)
	}
	for k, v := range updates {
		sess.SessionState[k] = v
	}
	_, err = w.cfg.DB.UpdateWorkflowSessionFields(ctx, db.SessionFieldsUpdate{SessionID: sessionID, ExpectedRevision: sess.Revision, SessionState: sess.SessionState})
	return err
}
