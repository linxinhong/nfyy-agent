// Package service provides shared HTTP orchestration helpers for AgentOS
// routers. It is the first slice of the service layer: the three component
// routers (agents/teams/workflows) previously duplicated run/resume/stream
// plumbing; these helpers collapse the identical pieces so future
// cross-cutting concerns (audit, rate limit, fine-grained authz) have one
// seam to attach to.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
)

// WriteSSE writes one server-sent event and flushes it.
func WriteSSE(w http.ResponseWriter, flusher http.Flusher, event string, data any) {
	fmt.Fprintf(w, "event: %s\n", event)
	if data != nil {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n", string(b))
	}
	fmt.Fprint(w, "\n")
	flusher.Flush()
}

// StreamEvents writes an agent event channel as SSE. The channel must be
// obtained by the caller from RunStreamWith/ResumeStream; this helper only
// owns the wire format.
func StreamEvents(w http.ResponseWriter, events <-chan agent.Event) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	for ev := range events {
		if name, data, ok := AgentFrame(ev, ""); ok {
			WriteSSE(w, flusher, name, data)
		}
	}
}

// AgentFrame maps an agent event to its SSE event name and payload. runID,
// when known, is reported in run_started so clients can reconnect.
func AgentFrame(ev agent.Event, runID string) (string, any, bool) {
	switch ev.Type {
	case agent.EventTypeRunStarted:
		if runID == "" {
			return "run_started", nil, true
		}
		return "run_started", map[string]string{"run_id": runID}, true
	case agent.EventTypeMessage:
		return "message", map[string]string{"content": ev.Content}, true
	case agent.EventTypeReasoning:
		return "reasoning", map[string]string{"content": ev.Content}, true
	case agent.EventTypeToolCall:
		payload := map[string]any{}
		if ev.ToolCall != nil {
			payload["id"] = ev.ToolCall.ID
			payload["name"] = ev.ToolCall.Function.Name
			payload["arguments"] = ev.ToolCall.Function.Arguments
		}
		return "tool_call", payload, true
	case agent.EventTypeToolStarted:
		payload := map[string]any{}
		if ev.ToolCall != nil {
			payload["id"] = ev.ToolCall.ID
			payload["name"] = ev.ToolCall.Function.Name
		}
		return "tool_started", payload, true
	case agent.EventTypeToolResult:
		payload := map[string]any{"result": ev.ToolResult}
		if ev.ToolCall != nil {
			payload["id"] = ev.ToolCall.ID
		}
		return "tool_result", payload, true
	case agent.EventTypeRunCompleted:
		return "run_completed", ev.RunOutput, true
	case agent.EventTypeRunResumed:
		return "run_resumed", ev.RunOutput, true
	case agent.EventTypeRunPaused:
		return "run_paused", ev.RunOutput, true
	case agent.EventTypeRunError:
		msg := "run failed"
		if ev.Err != nil {
			msg = ev.Err.Error()
		}
		return "run_error", map[string]string{"error": msg}, true
	}
	return "", nil, false
}

// StreamRun journals a run's frames in the hub and serves them to the
// initiating request. The run must have been started with a context
// detached from the request (cancel cancels it); a disconnect leaves it
// running for the hub's DisconnectGrace so the client can reconnect through
// GET /runs/{run_id}/events with Last-Event-ID. produce publishes frames
// and returns when the run's event stream is exhausted.
func (h *StreamHub) StreamRun(w http.ResponseWriter, req *http.Request, runID string, cancel context.CancelFunc, produce func(*Publisher)) {
	owner := OwnerFromRequest(req)
	pub := h.Start(runID, owner, cancel)
	go func() {
		defer cancel()
		defer pub.Close()
		produce(pub)
	}()
	_ = h.Serve(w, req, runID, owner, LastEventID(req))
}

// PublishAgentEvents publishes every agent event accepted by keep (nil
// keeps all) until the channel closes.
func PublishAgentEvents(pub *Publisher, runID string, events <-chan agent.Event, keep func(agent.Event) bool) {
	for ev := range events {
		if keep != nil && !keep(ev) {
			continue
		}
		if name, data, ok := AgentFrame(ev, runID); ok {
			pub.Publish(name, data)
		}
	}
}

// WriteJSON writes a JSON response.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// WriteError writes a JSON error envelope.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}
