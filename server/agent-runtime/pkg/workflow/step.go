// Package workflow implements sequential step-based workflow execution.
package workflow

import (
	"context"
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// StepInput is passed to each step when it executes.
type StepInput struct {
	// Input is the original or previous-step output passed to the step.
	Input string
	// PreviousOutput is the output of the immediately preceding step.
	PreviousOutput string
	// SessionState is the current workflow session state.
	SessionState map[string]any
}

// StepOutput is the result of executing a step.
type StepOutput struct {
	// Content is the textual result of the step.
	Content string
	// Parsed holds structured output when an output schema is used.
	Parsed any
}

// Step represents a single node in a workflow.
type Step struct {
	// Name is the step name.
	Name string
	// Agent runs this step when set.
	Agent *agent.Agent
	// Function runs this step when no Agent is set.
	Function func(context.Context, StepInput) (StepOutput, error)
}

// Steps is a convenience alias for a slice of Step values.
type Steps []Step

// run executes the step using its Agent or Function.
func (s Step) run(ctx context.Context, input StepInput, request session.RunRequest) (*StepOutput, error) {
	if s.Agent != nil {
		request.Input = session.RunInput{Message: input.Input}
		out, err := s.Agent.RunWith(ctx, request)
		if err != nil {
			return nil, err
		}
		return runOutputToStepOutput(out), nil
	}

	if s.Function != nil {
		out, err := s.Function(ctx, input)
		if err != nil {
			return nil, err
		}
		return &out, nil
	}

	return nil, fmt.Errorf("workflow step %q has no Agent or Function", s.Name)
}

func runOutputToStepOutput(out *session.RunOutput) *StepOutput {
	if out == nil {
		return nil
	}
	return &StepOutput{
		Content: out.Content,
		Parsed:  out.Parsed,
	}
}
