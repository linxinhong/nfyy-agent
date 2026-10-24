// Package workflow implements sequential step-based workflow execution.
package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/internal/streamtransport"
	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Workflow-specific event types emitted around individual steps.
const (
	// EventTypeStepStarted marks the start of a workflow step.
	EventTypeStepStarted agent.EventType = "step_started"
	// EventTypeStepCompleted marks the successful end of a workflow step.
	EventTypeStepCompleted agent.EventType = "step_completed"
)

// runStream executes the legacy workflow through the bounded emitter.
func (w *Workflow) runStream(ctx context.Context, input, requestedRunID string, metadata map[string]string, emit func(agent.Event, streamtransport.Kind) bool) (*session.RunOutput, error) {
	emit(agent.Event{Type: agent.EventTypeRunStarted}, streamtransport.Data)

	sess, err := w.loadOrCreateSession(ctx)
	if err != nil {
		ev := agent.Event{Type: agent.EventTypeRunError, Err: fmt.Errorf("load workflow session: %w", err)}
		emit(ev, streamtransport.Terminal)
		return nil, ev.Err
	}
	output := w.newRunOutput(sess, requestedRunID, input, metadata)
	if err := w.persistRun(ctx, output); err != nil {
		ev := agent.Event{Type: agent.EventTypeRunError, Err: fmt.Errorf("start workflow run: %w", err)}
		emit(ev, streamtransport.Terminal)
		return output, ev.Err
	}

	currentInput := input
	var previousOutput string
	var runErr error

	for i, step := range w.cfg.Steps {
		if err := ctx.Err(); err != nil {
			emit(agent.Event{Type: agent.EventTypeRunError, Err: err}, streamtransport.Terminal)
			output.Status = session.RunStatusCancelled
			output.Error = err.Error()
			runErr = err
			break
		}

		stepInput := StepInput{
			Input:          currentInput,
			PreviousOutput: previousOutput,
			SessionState:   sess.SessionState,
		}

		emit(agent.Event{Type: EventTypeStepStarted, Content: step.Name}, streamtransport.Data)
		stepStartedAt := time.Now().UTC()

		var stepOutput *StepOutput
		if step.Agent != nil {
			stepOutput, err = w.runAgentStepStream(ctx, step, stepInput, session.RunRequest{
				UserID:      output.UserID,
				ParentRunID: output.RunID,
				NodeID:      step.Name,
				Metadata:    output.Metadata,
			}, emit)
		} else if step.Function != nil {
			var out StepOutput
			out, err = step.Function(ctx, stepInput)
			if err == nil {
				stepOutput = &out
			}
		} else {
			err = fmt.Errorf("workflow step %q has no Agent or Function", step.Name)
		}

		if err != nil {
			output.Status = session.RunStatusError
			output.Error = err.Error()
			emit(agent.Event{Type: agent.EventTypeRunError, Err: err}, streamtransport.Terminal)
			runErr = err
			break
		}

		previousOutput = stepOutput.Content
		currentInput = stepOutput.Content
		stepRun := w.newStepRunOutput(sess, step, stepInput.Input, stepOutput, output, stepStartedAt)
		if i < len(w.cfg.Steps)-1 {
			if err := w.persistRun(ctx, stepRun); err != nil {
				output.Status = session.RunStatusError
				output.Error = err.Error()
				emit(agent.Event{Type: agent.EventTypeRunError, Err: err}, streamtransport.Terminal)
				runErr = err
				break
			}
		} else {
			output.AgentID = stepRun.AgentID
			output.Content = stepRun.Content
			output.Parsed = stepRun.Parsed
			stepRun.RunID = output.RunID
		}
		emit(agent.Event{Type: EventTypeStepCompleted, Content: step.Name, RunOutput: stepRun}, streamtransport.Data)
	}

	if runErr == nil {
		output.Status = session.RunStatusCompleted
	}
	if err := w.updateRun(ctx, output); err != nil {
		emit(agent.Event{Type: agent.EventTypeRunError, Err: fmt.Errorf("save workflow run: %w", err)}, streamtransport.Terminal)
		runErr = err
	} else if output.Status == session.RunStatusCompleted {
		emit(agent.Event{Type: agent.EventTypeRunCompleted, RunOutput: output}, streamtransport.Terminal)
	}

	return output, runErr
}

func (w *Workflow) runAgentStepStream(ctx context.Context, step Step, input StepInput, request session.RunRequest, emit func(agent.Event, streamtransport.Kind) bool) (*StepOutput, error) {
	request.Input = session.RunInput{Message: input.Input}
	stream, err := step.Agent.RunStreamWith(ctx, request)
	if err != nil {
		return nil, err
	}

	var output *session.RunOutput
	for ev := range stream {
		kind := streamtransport.Data
		if ev.Type == agent.EventTypeRunPaused {
			kind = streamtransport.Critical
		}
		emit(ev, kind)
		if ev.RunOutput != nil {
			output = ev.RunOutput
		}
	}

	if output == nil {
		return nil, fmt.Errorf("workflow step %q produced no output", step.Name)
	}

	return &StepOutput{Content: output.Content, Parsed: output.Parsed}, nil
}
