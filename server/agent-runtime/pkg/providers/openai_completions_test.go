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

func TestOpenAICompletionsResponse(t *testing.T) {
	var gotReq chatCompletionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse{
			ID:    "resp_1",
			Model: "gpt-4o",
			Choices: []choice{
				{Message: chatCompletionMessage{Role: "assistant", Content: "Hello"}, FinishReason: "stop"},
			},
			Usage: &usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		})
	}))
	defer srv.Close()

	m := Model{ID: "gpt-4o", API: APIOpenAICompletions, Provider: "openai", BaseURL: srv.URL}
	chat, err := m.Chat(ChatOptions{APIKey: "key"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	resp, err := chat.Response(context.Background(), []models.Message{
		{Role: models.RoleSystem, Content: "You are helpful"},
		{Role: models.RoleUser, Content: "Hi"},
	}, models.ModelOptions{})
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if gotReq.Model != "gpt-4o" {
		t.Errorf("request model = %q, want gpt-4o", gotReq.Model)
	}
	if len(gotReq.Messages) != 2 {
		t.Fatalf("request messages len = %d, want 2", len(gotReq.Messages))
	}
	if resp.Content != "Hello" {
		t.Errorf("content = %q, want Hello", resp.Content)
	}
	if resp.Usage.TotalTokens != 12 {
		t.Errorf("usage total = %d, want 12", resp.Usage.TotalTokens)
	}
}

func TestOpenAICompletionsReasoningAndParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse{
			Choices: []choice{
				{Message: chatCompletionMessage{
					Role:             "assistant",
					Content:          `{"answer":42}`,
					ReasoningContent: "thinking...",
				}, FinishReason: "stop"},
			},
			Usage: &usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		})
	}))
	defer srv.Close()

	m := Model{ID: "deepseek-reasoner", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: srv.URL}
	chat, _ := m.Chat(ChatOptions{APIKey: "key"})
	rf := &models.ResponseFormat{Type: models.ResponseFormatJSON}
	resp, err := chat.Response(context.Background(), []models.Message{
		{Role: models.RoleUser, Content: "hi"},
	}, models.ModelOptions{ResponseFormat: rf})
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if resp.ReasoningContent != "thinking..." {
		t.Errorf("reasoning = %q, want thinking...", resp.ReasoningContent)
	}
	parsed, ok := resp.Parsed.(map[string]any)
	if !ok {
		t.Fatalf("parsed type = %T, want map", resp.Parsed)
	}
	if parsed["answer"] != float64(42) {
		t.Errorf("parsed = %v", parsed)
	}
}

func TestOpenAICompletionsStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{
			`data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"thinking","content":"He"}}]}`,
			`data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
			`data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte(c + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	m := Model{ID: "gpt-4o", API: APIOpenAICompletions, Provider: "openai", BaseURL: srv.URL}
	chat, _ := m.Chat(ChatOptions{APIKey: "key"})
	ch, err := chat.ResponseStream(context.Background(), []models.Message{
		{Role: models.RoleUser, Content: "Hi"},
	}, models.ModelOptions{})
	if err != nil {
		t.Fatalf("ResponseStream: %v", err)
	}

	var content, reasoning string
	var totalTokens int
	var done bool
	for ev := range ch {
		switch ev.Event {
		case "content_delta":
			content += ev.Content
		case "reasoning_delta":
			reasoning += ev.ReasoningContent
		case "usage":
			if ev.Usage != nil {
				totalTokens = ev.Usage.TotalTokens
			}
		case "done":
			done = true
		case "error":
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if content != "Hello" {
		t.Errorf("content = %q, want Hello", content)
	}
	if reasoning != "thinking" {
		t.Errorf("reasoning = %q, want thinking", reasoning)
	}
	if totalTokens != 7 {
		t.Errorf("total tokens = %d, want 7", totalTokens)
	}
	if !done {
		t.Error("expected done event")
	}
}

func TestOpenAICompletionsJSONSchemaOnlyOnOfficialEndpoint(t *testing.T) {
	m := Model{ID: "gpt-4o", API: APIOpenAICompletions, Provider: "openai", BaseURL: ""}
	chat, _ := m.Chat(ChatOptions{APIKey: "key"})
	capable := chat.(models.JSONSchemaCapable).SupportsNativeJSONSchema()
	if !capable {
		t.Error("official endpoint should support native json_schema")
	}

	m2 := Model{ID: "ds", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: "https://api.deepseek.com/v1"}
	chat2, _ := m2.Chat(ChatOptions{APIKey: "key"})
	if chat2.(models.JSONSchemaCapable).SupportsNativeJSONSchema() {
		t.Error("deepseek endpoint should not claim native json_schema")
	}
}

// 思考模式契约：ctx 注入的思考强度应写入 reasoning_effort 与
// thinking.type=enabled；未注入时不携带这两个字段（保持 API 默认）。
func TestOpenAICompletionsReasoningEffortFromContext(t *testing.T) {
	var gotReq chatCompletionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = chatCompletionRequest{}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatCompletionResponse{
			ID: "resp_1", Model: "m",
			Choices: []choice{{Message: chatCompletionMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		})
	}))
	defer srv.Close()

	m := Model{ID: "m", API: APIOpenAICompletions, Provider: "openai", BaseURL: srv.URL}
	chat, err := m.Chat(ChatOptions{APIKey: "key"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	messages := []models.Message{{Role: models.RoleUser, Content: "hi"}}

	ctx := models.WithReasoningEffort(context.Background(), models.ReasoningEffortMax)
	if _, err = chat.Response(ctx, messages, models.ModelOptions{}); err != nil {
		t.Fatalf("Response: %v", err)
	}
	if gotReq.ReasoningEffort != "max" {
		t.Errorf("reasoning_effort = %q, want max", gotReq.ReasoningEffort)
	}
	if gotReq.Thinking == nil || gotReq.Thinking.Type != "enabled" {
		t.Errorf("thinking = %+v, want enabled", gotReq.Thinking)
	}

	if _, err = chat.Response(context.Background(), messages, models.ModelOptions{}); err != nil {
		t.Fatalf("Response without effort: %v", err)
	}
	if gotReq.ReasoningEffort != "" || gotReq.Thinking != nil {
		t.Errorf("no ctx injection must keep body clean, got effort=%q thinking=%+v", gotReq.ReasoningEffort, gotReq.Thinking)
	}

	if models.ValidReasoningEffort("ultra") {
		t.Error("ultra must not be a valid effort")
	}
}
