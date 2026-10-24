package workflow

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/internal/streamtransport"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

func streamEvents(ctx context.Context, cfg streamcfg.Config, operation func(context.Context, func(Event)) (*session.RunOutput, error)) <-chan Event {
	streamCtx, cancel := context.WithCancel(ctx)
	var sequence atomic.Uint64
	writer, ch := newWorkflowEventStream(streamCtx, cancel, cfg, sequence.Add)
	go func() {
		var runErr error
		defer func() { writer.Finish(runErr) }()
		var mu sync.Mutex
		emit := func(event Event) {
			mu.Lock()
			defer mu.Unlock()
			event.Sequence = sequence.Add(1)
			event.RunOutput = cloneRunOutput(event.RunOutput)
			writer.Emit(event, workflowEventKind(event))
		}
		_, runErr = operation(streamCtx, emit)
	}()
	return ch
}

func workflowEventKind(event Event) streamtransport.Kind {
	switch event.Type {
	case EventTypeRunPaused, EventTypeRunCompleted, EventTypeRunError, EventTypeRunCancelled:
		return streamtransport.Terminal
	case EventTypeRequirementCreated:
		return streamtransport.Critical
	default:
		return streamtransport.Data
	}
}

type EventType string

const (
	EventTypeRunStarted          EventType = "run_started"
	EventTypeNodeStarted         EventType = "node_started"
	EventTypeNodeCompleted       EventType = "node_completed"
	EventTypeNodePaused          EventType = "node_paused"
	EventTypeNodeError           EventType = "node_error"
	EventTypeRequirementCreated  EventType = "requirement_created"
	EventTypeRequirementResolved EventType = "requirement_resolved"
	EventTypeRunPaused           EventType = "run_paused"
	EventTypeRunResumed          EventType = "run_resumed"
	EventTypeRunCompleted        EventType = "run_completed"
	EventTypeRunError            EventType = "run_error"
	EventTypeRunCancelled        EventType = "run_cancelled"
)

type Event struct {
	Sequence     uint64             `json:"sequence"`
	Type         EventType          `json:"type"`
	RunID        string             `json:"run_id"`
	NodeID       string             `json:"node_id,omitempty"`
	ParentNodeID string             `json:"parent_node_id,omitempty"`
	Timestamp    time.Time          `json:"timestamp"`
	RunOutput    *session.RunOutput `json:"run_output,omitempty"`
	Payload      any                `json:"payload,omitempty"`
	Error        string             `json:"error,omitempty"`
}

func newEvent(kind EventType, output *session.RunOutput, nodeID, parentID string, err error) Event {
	event := Event{Type: kind, NodeID: nodeID, ParentNodeID: parentID, Timestamp: time.Now().UTC()}
	if output != nil {
		event.RunID = output.RunID
		switch kind {
		case EventTypeRunPaused, EventTypeRunResumed, EventTypeRunCompleted, EventTypeRunError, EventTypeRunCancelled:
			event.RunOutput = output
		}
	}
	if err != nil {
		event.Error = err.Error()
	}
	return event
}
