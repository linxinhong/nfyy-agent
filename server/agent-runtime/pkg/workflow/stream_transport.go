package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/internal/streamtransport"
	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

const workflowEventAccountingOverhead = 64

func newLegacyEventStream(ctx context.Context, cancel context.CancelFunc, cfg streamcfg.Config) (streamtransport.Writer[agent.Event], <-chan agent.Event) {
	return streamtransport.New(ctx, cancel, cfg, legacyEventBytes, func(cause error) agent.Event {
		return agent.Event{Type: agent.EventTypeRunError, Err: boundedWorkflowStreamError(cause)}
	})
}

func newWorkflowEventStream(ctx context.Context, cancel context.CancelFunc, cfg streamcfg.Config, nextSequence func(uint64) uint64) (streamtransport.Writer[Event], <-chan Event) {
	return streamtransport.New(ctx, cancel, cfg, workflowEventBytes, func(cause error) Event {
		return Event{Sequence: nextSequence(1), Type: EventTypeRunError, Timestamp: time.Now().UTC(), Error: boundedWorkflowStreamError(cause).Error()}
	})
}

func legacyEventBytes(event agent.Event, limit int) int {
	size := workflowEventAccountingOverhead + len(event.Type) + len(event.Content)
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

func workflowEventBytes(event Event, limit int) int {
	size := workflowEventAccountingOverhead + len(event.Type) + len(event.RunID) + len(event.NodeID) + len(event.ParentNodeID) + len(event.Error)
	if size > limit {
		return limit + 1
	}
	for _, value := range []any{event.RunOutput, event.Payload} {
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

func boundedWorkflowStreamError(cause error) error {
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
