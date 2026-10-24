package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

func TestAnthropicMessagesResponse(t *testing.T) {
	var gotReq anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if r.Header.Get("x-api-key") != "key" {
			t.Errorf("x-api-key = %q, want key", r.Header.Get("x-api-key"))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			ID:   "msg_1",
			Type: "message",
			Role: "assistant",
			Content: []anthropicBlock{
				{Type: "thinking", Thinking: "thinking..."},
				{Type: "text", Text: "Hello"},
			},
			Usage: &anthropicUsage{InputTokens: 10, OutputTokens: 2},
		})
	}))
	defer srv.Close()

	m := Model{ID: "claude-sonnet-4-20250514", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}
	chat, _ := m.Chat(ChatOptions{APIKey: "key"})
	resp, err := chat.Response(context.Background(), []models.Message{
		{Role: models.RoleUser, Content: "Hi"},
	}, models.ModelOptions{})
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if resp.Content != "Hello" {
		t.Errorf("content = %q, want Hello", resp.Content)
	}
	if resp.ReasoningContent != "thinking..." {
		t.Errorf("reasoning = %q, want thinking...", resp.ReasoningContent)
	}
	if resp.Usage.TotalTokens != 12 {
		t.Errorf("total tokens = %d, want 12", resp.Usage.TotalTokens)
	}
	if len(gotReq.Messages) != 1 || gotReq.Messages[0].Role != "user" {
		t.Errorf("messages = %+v", gotReq.Messages)
	}
}

func TestAnthropicMessagesToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Type: "message",
			Role: "assistant",
			Content: []anthropicBlock{
				{Type: "tool_use", ID: "toolu_1", Name: "get_weather", Input: map[string]any{"city": "Paris"}},
			},
			Usage: &anthropicUsage{InputTokens: 1, OutputTokens: 2},
		})
	}))
	defer srv.Close()

	m := Model{ID: "claude", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}
	chat, _ := m.Chat(ChatOptions{APIKey: "key"})
	opts := models.ModelOptions{Tools: []models.Tool{{
		Type: "function",
		Function: models.ToolFunction{
			Name:        "get_weather",
			Description: "weather",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}},
		},
	}}}
	resp, err := chat.Response(context.Background(), []models.Message{
		{Role: models.RoleUser, Content: "weather?"},
	}, opts)
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool call = %+v", tc)
	}
}

func TestAnthropicMessagesStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		events := buildAnthropicStreamEvents([]anthropicStreamEvent{
			{Type: "message_start", Message: &anthropicResponse{Usage: &anthropicUsage{InputTokens: 10}}},
			{Type: "content_block_start", Index: 0, ContentBlock: &anthropicBlock{Type: "text"}},
			{Type: "content_block_delta", Index: 0, Delta: &anthropicDelta{Type: "text_delta", Text: "Hello"}},
			{Type: "content_block_start", Index: 1, ContentBlock: &anthropicBlock{Type: "tool_use", ID: "toolu_1", Name: "get_weather"}},
			{Type: "content_block_delta", Index: 1, Delta: &anthropicDelta{Type: "input_json_delta", PartialJSON: `{`}},
			{Type: "content_block_delta", Index: 1, Delta: &anthropicDelta{Type: "input_json_delta", PartialJSON: `"city":"`}},
			{Type: "content_block_delta", Index: 1, Delta: &anthropicDelta{Type: "input_json_delta", PartialJSON: `Paris`}},
			{Type: "content_block_delta", Index: 1, Delta: &anthropicDelta{Type: "input_json_delta", PartialJSON: `"}`}},
			{Type: "message_delta", Usage: &anthropicUsage{OutputTokens: 5}},
			{Type: "message_stop"},
		})
		for _, e := range events {
			_, _ = w.Write([]byte(e + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	m := Model{ID: "claude", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}
	chat, _ := m.Chat(ChatOptions{APIKey: "key"})
	ch, err := chat.ResponseStream(context.Background(), []models.Message{
		{Role: models.RoleUser, Content: "Hi"},
	}, models.ModelOptions{})
	if err != nil {
		t.Fatalf("ResponseStream: %v", err)
	}

	var content string
	var args string
	var usage *models.MessageMetrics
	var done bool
	for ev := range ch {
		switch ev.Event {
		case "content_delta":
			content += ev.Content
		case "tool_call_delta":
			if ev.ToolCall != nil {
				args += ev.ToolCall.Function.Arguments
			}
		case "usage":
			usage = ev.Usage
		case "done":
			done = true
		case "error":
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if content != "Hello" {
		t.Errorf("content = %q, want Hello", content)
	}
	if args != `{"city":"Paris"}` {
		t.Errorf("tool args = %q, want {\"city\":\"Paris\"}", args)
	}
	if usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", usage)
	}
	if !done {
		t.Error("expected done")
	}
}

func TestAnthropicMessagesToolResultRoundTrip(t *testing.T) {
	var gotReq anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Type:    "message",
			Role:    "assistant",
			Content: []anthropicBlock{{Type: "text", Text: "ok"}},
			Usage:   &anthropicUsage{InputTokens: 1, OutputTokens: 1},
		})
	}))
	defer srv.Close()

	m := Model{ID: "claude", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}
	chat, _ := m.Chat(ChatOptions{APIKey: "key"})
	resp, err := chat.Response(context.Background(), []models.Message{
		{Role: models.RoleAssistant, Content: "", ToolCalls: []models.ToolCall{{
			ID:   "toolu_1",
			Type: "function",
			Function: models.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"city":"Paris"}`,
			},
		}}},
		{Role: models.RoleTool, Content: "sunny", ToolCallID: "toolu_1"},
	}, models.ModelOptions{})
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q, want ok", resp.Content)
	}
	if len(gotReq.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(gotReq.Messages))
	}
	if gotReq.Messages[1].Role != "user" || len(gotReq.Messages[1].Content) != 1 {
		t.Fatalf("tool result message = %+v", gotReq.Messages[1])
	}
	if gotReq.Messages[1].Content[0].ToolUseID != "toolu_1" {
		t.Errorf("tool_use_id = %q, want toolu_1", gotReq.Messages[1].Content[0].ToolUseID)
	}
}

func buildAnthropicStreamEvents(events []anthropicStreamEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			panic(err)
		}
		out[i] = "data: " + string(b)
	}
	return out
}
