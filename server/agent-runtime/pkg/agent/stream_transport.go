package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/internal/streamtransport"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

const agentEventAccountingOverhead = 64

var (
	agentRunOutputMeasurer   = streamtransport.NewLogicalPayloadMeasurer[*session.RunOutput]()
	agentRequirementMeasurer = streamtransport.NewLogicalPayloadMeasurer[*requirement.Requirement]()
)

// agentEventBytes charges logical payload bytes plus a fixed event overhead.
// Composite values follow streamtransport.LogicalPayloadBytes; this is not a
// wire-size, backing-capacity, or process-memory measurement.
func agentEventBytes(event Event, limit int) int {
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
	var ok bool
	if event.ToolResult != nil {
		size, ok = addAgentPayloadBytes(size, streamtransport.LogicalPayloadBytes(event.ToolResult, limit-size), limit)
		if !ok {
			return size
		}
	}
	size, ok = addAgentPayloadBytes(size, agentRunOutputMeasurer.Bytes(event.RunOutput, limit-size), limit)
	if !ok {
		return size
	}
	size, ok = addAgentPayloadBytes(size, agentRequirementMeasurer.Bytes(event.Requirement, limit-size), limit)
	if !ok {
		return size
	}
	return size
}

func addAgentPayloadBytes(size, payloadSize, limit int) (int, bool) {
	if payloadSize < 0 {
		return -1, false
	}
	if payloadSize > limit-size {
		return limit + 1, false
	}
	return size + payloadSize, true
}

func boundedStreamError(cause error) error {
	switch {
	case errors.Is(cause, streamcfg.ErrOutputBudgetExceeded):
		return streamcfg.ErrOutputBudgetExceeded
	case errors.Is(cause, streamcfg.ErrDeliveryTimeout):
		return streamcfg.ErrDeliveryTimeout
	case errors.Is(cause, context.Canceled):
		return context.Canceled
	case errors.Is(cause, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return fmt.Errorf("%w: %.96s", streamcfg.ErrEndedWithoutTerminal, cause.Error())
	}
}
