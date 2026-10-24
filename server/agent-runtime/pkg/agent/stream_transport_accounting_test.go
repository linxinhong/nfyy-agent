package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/internal/streamtransport"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestAgentEventBytesMatchesFrozenAccounting(t *testing.T) {
	var typedNil *perfToolResult
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	now := time.Now().UTC()
	events := []Event{
		{},
		{Type: EventTypeMessage, Content: "content"},
		{Type: EventTypeRunError, Err: errors.New("bounded failure")},
		{Type: EventTypeToolResult, ToolResult: typedNil},
		{Type: EventTypeToolResult, ToolResult: perfToolResult{Name: "result", Values: []int64{1, 2, 3}}},
		{Type: EventTypeToolResult, ToolResult: cyclic},
		{Type: EventTypeToolResult, ToolResult: func() {}},
		{Type: EventTypeRunCompleted, RunOutput: &session.RunOutput{
			RunID: "run", AgentID: "agent", Content: "done", Status: session.RunStatusCompleted,
			Metadata:  map[string]string{"trace": "fixed"},
			Input:     session.RunInput{Message: "input", Images: []models.Image{{URL: "image"}}},
			Parsed:    map[string]any{"answer": []int64{1, 2, 3}},
			CreatedAt: now, UpdatedAt: now,
		}},
		{Type: EventTypeRunPaused, Requirement: &requirement.Requirement{
			ID: "requirement", RunID: "run", Kind: requirement.KindToolApproval,
			ToolCall: models.ToolCall{ID: "call", Type: "function", Function: models.FunctionCall{Name: "tool", Arguments: `{}`}},
			Status:   requirement.StatusPending, CreatedAt: now,
		}},
	}
	for _, event := range events {
		full := frozenAgentEventBytes(event, 1<<20)
		limits := []int{0, 1, 63, 64, 128, 255, 256, 512, 1 << 20}
		if full >= 0 && full < 1<<20 {
			limits = append(limits, full-1, full, full+1)
		}
		for _, limit := range limits {
			if got, want := agentEventBytes(event, limit), frozenAgentEventBytes(event, limit); got != want {
				t.Fatalf("event %s at limit %d size = %d, want frozen %d; event=%+v", event.Type, limit, got, want, event)
			}
		}
	}
}

func TestAgentEventBytesPreservesPERF001LogicalSizes(t *testing.T) {
	var typedNil *perfToolResult
	tests := []struct {
		name  string
		event Event
		want  int
	}{
		{name: "content", event: Event{Type: EventTypeMessage, Content: "fixed-content-delta"}, want: 106},
		{name: "tool result", event: Event{Type: EventTypeToolResult, ToolResult: perfToolResult{
			Name: "fixed-tool-result", Values: []int64{1, 2, 3, 5, 8, 13, 21, 34},
		}}, want: 252},
		{name: "completed", event: Event{Type: EventTypeRunCompleted, Done: true, RunOutput: &session.RunOutput{RunID: "perf-run", AgentID: "perf-agent", Content: "done", Status: session.RunStatusCompleted}}, want: 1804}, // 1756 + RunOutput.LeaseTimeout and RecoveredBy fields
		{name: "paused", event: Event{Type: EventTypeRunPaused, Requirement: &requirement.Requirement{ID: "perf-requirement", RunID: "perf-run", Kind: requirement.KindToolApproval, ToolCall: models.ToolCall{ID: "perf-call", Type: "function", Function: models.FunctionCall{Name: "fixed-tool", Arguments: `{}`}}, Status: requirement.StatusPending}}, want: 819},
		{name: "typed nil", event: Event{Type: EventTypeToolResult, ToolResult: typedNil}, want: 99},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := agentEventBytes(test.event, perfEventLimit); got != test.want {
				t.Fatalf("logical size = %d, want PERF-001 frozen %d", got, test.want)
			}
		})
	}
}

// frozenAgentEventBytes is the pre-PERF-002 event accounting path. The
// generic logical-byte walker is guarded independently by streamtransport's
// frozen reference differential test.
func frozenAgentEventBytes(event Event, limit int) int {
	size := agentEventAccountingOverhead + len(event.Type) + len(event.Content)
	if event.Err != nil {
		size += len(event.Err.Error())
	}
	if event.ToolCall != nil {
		size += len(event.ToolCall.ID) + len(event.ToolCall.Type)
		size += len(event.ToolCall.Function.Name) + len(event.ToolCall.Function.Arguments) + 32
	}
	if size > limit {
		return limit + 1
	}
	for _, value := range []any{event.ToolResult, event.RunOutput, event.Requirement} {
		if value == nil {
			continue
		}
		payloadSize := streamtransport.LogicalPayloadBytes(value, limit-size)
		if payloadSize < 0 {
			return -1
		}
		if payloadSize > limit-size {
			return limit + 1
		}
		size += payloadSize
	}
	return size
}
