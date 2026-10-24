//go:build go1.24

package agent

import (
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

const perfEventLimit = 1 << 20

var perfEventBytes int

type perfToolResult struct {
	Name   string
	Values []int64
}

// BenchmarkPerfAgentEventBytes records the actual event shapes that choose
// the direct string path or the reflective composite path.
func BenchmarkPerfAgentEventBytes(b *testing.B) {
	var typedNil *perfToolResult
	cases := []struct {
		name  string
		event Event
	}{
		{name: "content_delta", event: Event{Type: EventTypeMessage, Content: "fixed-content-delta"}},
		{name: "tool_result_composite", event: Event{Type: EventTypeToolResult, ToolResult: perfToolResult{
			Name: "fixed-tool-result", Values: []int64{1, 2, 3, 5, 8, 13, 21, 34},
		}}},
		{name: "run_completed", event: Event{Type: EventTypeRunCompleted, Done: true, RunOutput: &session.RunOutput{
			RunID: "perf-run", AgentID: "perf-agent", Content: "done", Status: session.RunStatusCompleted,
		}}},
		{name: "run_paused", event: Event{Type: EventTypeRunPaused, Requirement: &requirement.Requirement{
			ID: "perf-requirement", RunID: "perf-run", Kind: requirement.KindToolApproval,
			ToolCall: models.ToolCall{ID: "perf-call", Type: "function", Function: models.FunctionCall{Name: "fixed-tool", Arguments: `{}`}},
			Status:   requirement.StatusPending,
		}}},
		{name: "typed_nil_tool_result", event: Event{Type: EventTypeToolResult, ToolResult: typedNil}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			want := agentEventBytes(tc.event, perfEventLimit)
			if want < 0 || want > perfEventLimit {
				b.Fatalf("invalid fixed event size %d", want)
			}
			b.ReportAllocs()
			for b.Loop() {
				perfEventBytes = agentEventBytes(tc.event, perfEventLimit)
			}
			b.ReportMetric(float64(want), "logical-bytes/event")
		})
	}
}
