// Package workflow implements sequential step-based workflow execution.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// run executes the workflow sequentially.
func (w *Workflow) run(ctx context.Context, input, requestedRunID string, metadata map[string]string) (*session.RunOutput, error) {
	sess, err := w.loadOrCreateSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workflow session: %w", err)
	}

	output := w.newRunOutput(sess, requestedRunID, input, metadata)
	w.markLease(output)
	if err := w.persistRun(ctx, output); err != nil {
		return output, fmt.Errorf("start workflow run: %w", err)
	}
	ctx, _, stopLease := w.guardLease(ctx, output)
	defer stopLease()

	currentInput := input
	var previousOutput string

	for i, step := range w.cfg.Steps {
		if ctx.Err() != nil {
			err := cancelledError(ctx)
			output.Status = session.RunStatusCancelled
			output.Error = err.Error()
			if persistErr := w.updateRun(ctx, output); persistErr != nil {
				return output, fmt.Errorf("save cancelled workflow run: %w", persistErr)
			}
			return output, classifyExecutionError(err)
		}

		stepInput := StepInput{
			Input:          currentInput,
			PreviousOutput: previousOutput,
			SessionState:   sess.SessionState,
		}

		stepStartedAt := time.Now().UTC()
		stepOutput, err := step.run(ctx, stepInput, session.RunRequest{
			UserID:      output.UserID,
			ParentRunID: output.RunID,
			NodeID:      step.Name,
			Metadata:    output.Metadata,
		})
		if err != nil && ctx.Err() != nil {
			cause := cancelledError(ctx)
			output.Status = session.RunStatusCancelled
			output.Error = cause.Error()
			if persistErr := w.updateRun(ctx, output); persistErr != nil {
				return output, errors.Join(cause, persistErr)
			}
			return output, classifyExecutionError(cause)
		}
		if err != nil {
			output.Status = session.RunStatusError
			output.Error = err.Error()
			runErr := fmt.Errorf("execute step %q: %w", step.Name, err)
			if persistErr := w.updateRun(ctx, output); persistErr != nil {
				return output, errors.Join(runErr, persistErr)
			}
			return output, classifyExecutionError(runErr)
		}

		previousOutput = stepOutput.Content
		currentInput = stepOutput.Content
		stepRun := w.newStepRunOutput(sess, step, stepInput.Input, stepOutput, output, stepStartedAt)
		if i < len(w.cfg.Steps)-1 {
			if err := w.persistRun(ctx, stepRun); err != nil {
				output.Status = session.RunStatusError
				output.Error = err.Error()
				return output, errors.Join(fmt.Errorf("save workflow step run: %w", err), w.updateRun(ctx, output))
			}
		} else {
			output.AgentID = stepRun.AgentID
			output.Content = stepRun.Content
			output.Parsed = stepRun.Parsed
		}
	}

	output.Status = session.RunStatusCompleted
	if err := w.updateRun(ctx, output); err != nil {
		return output, fmt.Errorf("save workflow run: %w", err)
	}
	return output, nil
}

func (w *Workflow) persistRun(ctx context.Context, output *session.RunOutput) error {
	if w.cfg.DB == nil {
		return nil
	}
	return w.cfg.DB.AppendRun(ctx, output)
}

func (w *Workflow) updateRun(ctx context.Context, output *session.RunOutput) error {
	if output != nil && !output.CreatedAt.IsZero() {
		output.Metrics.DurationMS = time.Since(output.CreatedAt).Milliseconds()
	}
	if w.cfg.DB == nil {
		return nil
	}
	return runmanager.Finalize(ctx, w.cfg.DB, output, session.RunStatusRunning)
}

func (w *Workflow) newRunOutput(sess *session.WorkflowSession, requestedRunID, input string, metadata map[string]string) *session.RunOutput {
	runID := requestedRunID
	if runID == "" {
		runID = uuid.NewString()
	}
	return &session.RunOutput{
		RunID:      runID,
		WorkflowID: w.ID(),
		SessionID:  sess.SessionID,
		UserID:     w.cfg.UserID,
		Metadata:   metadata,
		Input:      session.RunInput{Message: input},
		Status:     session.RunStatusRunning,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
}

func (w *Workflow) newStepRunOutput(sess *session.WorkflowSession, step Step, input string, out *StepOutput, parent *session.RunOutput, startedAt time.Time) *session.RunOutput {
	now := time.Now().UTC()
	output := &session.RunOutput{
		RunID:       uuid.NewString(),
		WorkflowID:  w.ID(),
		SessionID:   sess.SessionID,
		UserID:      w.cfg.UserID,
		ParentRunID: parent.RunID,
		NodeID:      step.Name,
		Metadata:    parent.Metadata,
		Input:       session.RunInput{Message: input},
		Status:      session.RunStatusCompleted,
		CreatedAt:   startedAt,
		UpdatedAt:   now,
	}
	output.Metrics.DurationMS = time.Since(startedAt).Milliseconds()
	if step.Agent != nil {
		output.AgentID = step.Agent.ID()
	}
	if out != nil {
		output.Content = out.Content
		output.Parsed = out.Parsed
	}
	return output
}

func (w *Workflow) loadOrCreateSession(ctx context.Context) (*session.WorkflowSession, error) {
	if w.cfg.DB == nil {
		return w.newSession(), nil
	}

	if w.cfg.SessionID != "" {
		sess, err := w.cfg.DB.ReadWorkflowSession(ctx, w.cfg.SessionID)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			return sess, nil
		}
	}

	sess := w.newSession()
	if err := w.cfg.DB.CreateWorkflowSession(ctx, sess); err != nil {
		return nil, err
	}
	w.cfg.SessionID = sess.SessionID
	return sess, nil
}

func (w *Workflow) newSession() *session.WorkflowSession {
	now := time.Now().UTC()
	sess := &session.WorkflowSession{
		SessionID:  uuid.NewString(),
		WorkflowID: w.ID(),
		UserID:     w.cfg.UserID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if len(w.cfg.SessionState) > 0 {
		sess.SessionState = make(map[string]any, len(w.cfg.SessionState))
		for k, v := range w.cfg.SessionState {
			sess.SessionState[k] = v
		}
	}
	return sess
}

func (w *Workflow) saveSession(ctx context.Context, sess *session.WorkflowSession) error {
	if w.cfg.DB == nil {
		return nil
	}
	existing, err := w.cfg.DB.ReadWorkflowSession(ctx, sess.SessionID)
	if err != nil {
		return err
	}
	if existing == nil {
		return w.cfg.DB.CreateWorkflowSession(ctx, sess)
	}
	return w.cfg.DB.UpdateWorkflowSession(ctx, sess)
}
