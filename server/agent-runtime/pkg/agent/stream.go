// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"strings"

	"github.com/linxinhong/xingding-agent-runtime/internal/streamtransport"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

func emitAgentEvent(writer *streamtransport.Writer[Event], event Event) bool {
	return writer.Emit(event, agentEventKind(event))
}

// streamAggregator collects content and tool-call deltas into a single
// assistant Message.
type streamAggregator struct {
	content   strings.Builder
	reasoning strings.Builder
	toolCalls map[int]*models.ToolCall
	toolArgs  map[int]*strings.Builder
}

func newStreamAggregator() *streamAggregator {
	return &streamAggregator{toolCalls: make(map[int]*models.ToolCall), toolArgs: make(map[int]*strings.Builder)}
}

func (agg *streamAggregator) addContent(delta string) {
	agg.content.WriteString(delta)
}

func (agg *streamAggregator) addReasoning(delta string) {
	agg.reasoning.WriteString(delta)
}

func (agg *streamAggregator) addToolCallDelta(delta *models.ToolCall) {
	if delta == nil {
		return
	}
	tc, ok := agg.toolCalls[delta.Index]
	if !ok {
		tc = &models.ToolCall{Index: delta.Index}
		agg.toolCalls[delta.Index] = tc
		agg.toolArgs[delta.Index] = &strings.Builder{}
	}
	if delta.ID != "" {
		tc.ID = delta.ID
	}
	if delta.Type != "" {
		tc.Type = delta.Type
	}
	if delta.Function.Name != "" {
		tc.Function.Name += delta.Function.Name
	}
	agg.toolArgs[delta.Index].WriteString(delta.Function.Arguments)
}

func (agg *streamAggregator) message() models.Message {
	msg := models.Message{
		Role:             models.RoleAssistant,
		Content:          agg.content.String(),
		ReasoningContent: agg.reasoning.String(),
	}

	if len(agg.toolCalls) == 0 {
		return msg
	}

	// Tool calls are ordered by index.
	maxIdx := -1
	for idx := range agg.toolCalls {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	msg.ToolCalls = make([]models.ToolCall, 0, len(agg.toolCalls))
	for i := 0; i <= maxIdx; i++ {
		if tc, ok := agg.toolCalls[i]; ok {
			call := *tc
			call.Function.Arguments = agg.toolArgs[i].String()
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	return msg
}
