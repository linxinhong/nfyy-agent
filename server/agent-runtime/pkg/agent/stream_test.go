package agent

import (
	"context"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

func TestAgent_RunStream_ContentOnly(t *testing.T) {
	m := mock.New()
	m.StreamEvents = []models.StreamEvent{
		{Event: "content_delta", Content: "Hello"},
		{Event: "content_delta", Content: " world"},
		{Event: "done", Done: true},
	}

	ag, err := New(AgentConfig{
		Name:  "stream-test",
		Model: m,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ch, err := ag.RunStream(context.Background(), "hi")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var gotStarted, gotMessage, gotCompleted bool
	var content string
	for ev := range ch {
		switch ev.Type {
		case EventTypeRunStarted:
			gotStarted = true
		case EventTypeMessage:
			content += ev.Content
			gotMessage = true
		case EventTypeRunCompleted:
			gotCompleted = true
			if ev.RunOutput == nil || ev.RunOutput.Content != "Hello world" {
				t.Fatalf("unexpected output: %+v", ev.RunOutput)
			}
		case EventTypeRunError:
			t.Fatalf("unexpected error: %v", ev.Err)
		}
	}

	if !gotStarted || !gotMessage || !gotCompleted {
		t.Fatalf("events missing: started=%v message=%v completed=%v", gotStarted, gotMessage, gotCompleted)
	}
	if content != "Hello world" {
		t.Fatalf("content = %q, want Hello world", content)
	}
}

func TestAgent_RunStream_ToolCall(t *testing.T) {
	m := mock.New()
	m.StreamEventsPerCall = [][]models.StreamEvent{
		{
			{Event: "tool_call_delta", ToolCall: &models.ToolCall{Index: 0, ID: "call_1", Type: "function", Function: models.FunctionCall{Name: "add"}}},
			{Event: "tool_call_delta", ToolCall: &models.ToolCall{Index: 0, Function: models.FunctionCall{Arguments: `{"a":2}`}}},
			{Event: "tool_call_delta", ToolCall: &models.ToolCall{Index: 0, Function: models.FunctionCall{Arguments: `,"b":3}`}}},
			{Event: "done", Done: true},
		},
		{
			{Event: "content_delta", Content: "Done"},
			{Event: "done", Done: true},
		},
	}

	addTool, err := tools.NewFunction("add", "Add two numbers.", addToolFunc)
	if err != nil {
		t.Fatalf("NewFunction: %v", err)
	}

	ag, err := New(AgentConfig{
		Name:         "stream-tool-test",
		Model:        m,
		Tools:        []tools.Tool{addTool},
		MaxToolCalls: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ch, err := ag.RunStream(context.Background(), "add")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var gotToolCall, gotToolResult, gotCompleted bool
	var finalContent string
	for ev := range ch {
		switch ev.Type {
		case EventTypeToolCall:
			gotToolCall = true
		case EventTypeToolResult:
			gotToolResult = true
		case EventTypeMessage:
			finalContent += ev.Content
		case EventTypeRunCompleted:
			gotCompleted = true
		case EventTypeRunError:
			t.Fatalf("unexpected error: %v", ev.Err)
		}
	}

	if !gotToolCall || !gotToolResult || !gotCompleted {
		t.Fatalf("events missing: tool_call=%v tool_result=%v completed=%v", gotToolCall, gotToolResult, gotCompleted)
	}
	if finalContent != "Done" {
		t.Fatalf("final content = %q, want Done", finalContent)
	}
}

func TestAgent_RunStream_StructuredOutput(t *testing.T) {
	m := mock.New()
	m.StreamEvents = []models.StreamEvent{
		{Event: "content_delta", Content: `{"city":"Berlin","temperature":20,"unit":"C"}`},
		{Event: "done", Done: true},
	}

	ag, err := New(AgentConfig{
		Name:         "stream-structured-test",
		Model:        m,
		OutputSchema: weatherResponse{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ch, err := ag.RunStream(context.Background(), "weather")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var output *session.RunOutput
	for ev := range ch {
		if ev.Type == EventTypeRunCompleted {
			output = ev.RunOutput
		}
	}

	if output == nil {
		t.Fatal("no run_completed event")
	}
	parsed, ok := output.Parsed.(weatherResponse)
	if !ok {
		t.Fatalf("expected weatherResponse, got %T", output.Parsed)
	}
	if parsed.City != "Berlin" {
		t.Fatalf("unexpected city: %s", parsed.City)
	}
}
