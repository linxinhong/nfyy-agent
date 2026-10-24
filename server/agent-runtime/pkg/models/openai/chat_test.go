package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

func TestResponseSerialization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var gotReq map[string]any
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_1",
			"model":  "gpt-4o",
			"object": "chat.completion",
			"choices": []any{
				map[string]any{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "Hello",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 2,
				"total_tokens":      12,
			},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	chat := NewChat("gpt-4o", "test-key").SetBaseURL(srv.URL)
	msgs := []models.Message{
		{Role: models.RoleSystem, Content: "You are helpful"},
		{Role: models.RoleUser, Content: "Hi"},
	}

	resp, err := chat.Response(context.Background(), msgs, models.ModelOptions{})
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if resp.Content != "Hello" {
		t.Errorf("response content = %q, want Hello", resp.Content)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 12 {
		t.Errorf("response usage = %+v, want total 12", resp.Usage)
	}
	if chat.Provider() != "openai" {
		t.Errorf("provider = %q, want openai", chat.Provider())
	}
}

func TestStreamParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flushing")
		}

		chunks := []string{
			`data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"He"}}]}`,
			`data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
			`data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			if _, err := w.Write([]byte(c + "\n\n")); err != nil {
				t.Fatalf("write chunk: %v", err)
			}
			flusher.Flush()
		}
	}))
	defer srv.Close()

	chat := NewChat("gpt-4o", "test-key").SetBaseURL(srv.URL)
	ch, err := chat.ResponseStream(context.Background(), []models.Message{
		{Role: models.RoleUser, Content: "Hi"},
	}, models.ModelOptions{})
	if err != nil {
		t.Fatalf("ResponseStream: %v", err)
	}

	var content string
	var usage *models.MessageMetrics
	var done bool
	for ev := range ch {
		switch ev.Event {
		case "content_delta":
			content += ev.Content
		case "usage":
			usage = ev.Usage
		case "done":
			done = true
		case "error":
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	if content != "Hello" {
		t.Errorf("streamed content = %q, want Hello", content)
	}
	if usage == nil || usage.TotalTokens != 7 {
		t.Errorf("streamed usage = %+v, want total 7", usage)
	}
	if !done {
		t.Error("expected final done event")
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var gotReq map[string]any
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_2",
			"model":  "gpt-4o",
			"object": "chat.completion",
			"choices": []any{
				map[string]any{
					"index": 0,
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []any{
							map[string]any{
								"id":   "call_1",
								"type": "function",
								"function": map[string]any{
									"name":      "get_weather",
									"arguments": `{"city":"Paris"}`,
								},
							},
						},
					},
					"finish_reason": "tool_calls",
				},
			},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	opts := models.ModelOptions{
		Tools: []models.Tool{
			{
				Type: "function",
				Function: models.ToolFunction{
					Name:        "get_weather",
					Description: "Get the weather for a city",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"city": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}

	chat := NewChat("gpt-4o", "test-key").SetBaseURL(srv.URL)
	resp, err := chat.Response(context.Background(), []models.Message{
		{Role: models.RoleUser, Content: "What's the weather in Paris?"},
	}, opts)
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("response tool calls len = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("function call = %+v", tc.Function)
	}
}
