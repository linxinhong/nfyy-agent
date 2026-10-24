// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Event is emitted during a streaming agent run.
type Event struct {
	// Type categorizes the event.
	Type EventType
	// Content carries text content for message, reasoning, or error events.
	Content string
	// ToolCall carries a tool-call delta when Type is tool_call.
	ToolCall *models.ToolCall
	// ToolResult carries the result of a tool call when Type is tool_result.
	ToolResult any
	// RunOutput carries the final run output when Type is run_completed.
	RunOutput *session.RunOutput
	// Err carries an error when Type is run_error.
	Err error
	// Done indicates the stream has finished.
	Done bool
	// Requirement carries the pending requirement when Type is run_paused.
	Requirement *requirement.Requirement
}

// EventType categorizes agent run events.
type EventType string

const (
	// EventTypeRunStarted marks the start of a run.
	EventTypeRunStarted EventType = "run_started"
	// EventTypeMessage carries a content delta from the model.
	EventTypeMessage EventType = "message"
	// EventTypeReasoning carries a reasoning delta from the model.
	EventTypeReasoning EventType = "reasoning"
	// EventTypeToolCall carries a tool-call delta.
	EventTypeToolCall EventType = "tool_call"
	// EventTypeToolStarted marks that a complete tool call began executing.
	EventTypeToolStarted EventType = "tool_started"
	// EventTypeToolResult carries the result of a tool call.
	EventTypeToolResult EventType = "tool_result"
	// EventTypeRunCompleted marks the successful end of a run.
	EventTypeRunCompleted EventType = "run_completed"
	// EventTypeRunError marks that a run failed.
	EventTypeRunError EventType = "run_error"
	// EventTypeRunPaused marks that a run is paused waiting for tool approval.
	EventTypeRunPaused EventType = "run_paused"
	// EventTypeRunResumed marks that a paused run has resumed.
	EventTypeRunResumed EventType = "run_resumed"
)
