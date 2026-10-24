package llmkernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/token"
)

func toolTestClaims() *token.Claims {
	return &token.Claims{Subject: "usr_1", WorkspaceID: "finance", Audience: "llm-gateway"}
}

func noopDLP() DLPEngine {
	return scanFunc(func(_ context.Context, text string) (DLPResult, error) {
		return DLPResult{Redacted: text}, nil
	})
}

type scanFunc func(ctx context.Context, text string) (DLPResult, error)

func (f scanFunc) Scan(ctx context.Context, text string) (DLPResult, error) {
	return f(ctx, text)
}

func allowPolicy() PolicyEngine {
	return allowPolicyEngine{}
}

type allowPolicyEngine struct{}

func (allowPolicyEngine) Decide(context.Context, PolicyInput) (Decision, error) {
	return Decision{Allowed: true, Effect: EffectAllow}, nil
}

// The routed client forwards tools/tool_choice and tool messages byte-faithfully.
func TestOpenAICompatClientToolPassthrough(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": null, "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "git_read_file", "arguments": "{\"repo\":\"acme/app\"}"}}]}, "finish_reason": "tool_calls"}], "usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}}`))
	}))
	t.Cleanup(upstream.Close)

	client := &OpenAICompatClient{}
	req := ChatRequest{
		Model: "stub-model",
		Messages: []ChatMessage{
			{Role: "user", Content: "read the file"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "git_read_file", Arguments: "{\"repo\":\"acme/app\"}"}}},
			{Role: "tool", Content: "file body", ToolCallID: "call_1"},
		},
		Tools:      json.RawMessage(`[{"type":"function","function":{"name":"git_read_file","description":"read"}}]`),
		ToolChoice: json.RawMessage(`"auto"`),
	}
	result, err := client.Chat(context.Background(), ProviderTarget{Name: "t", BaseURL: upstream.URL}, "cred", req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if gotBody["tools"] == nil || gotBody["tool_choice"] != "auto" {
		t.Fatalf("tools/tool_choice must pass through: %v", gotBody)
	}
	msgs := gotBody["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	if assistant["tool_calls"] == nil || assistant["tool_calls"].([]any)[0].(map[string]any)["id"] != "call_1" {
		t.Fatalf("assistant tool_calls must pass through: %v", assistant)
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id must pass through: %v", toolMsg)
	}

	if result.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", result.FinishReason)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].ID != "call_1" ||
		result.ToolCalls[0].Name != "git_read_file" || result.ToolCalls[0].Arguments != `{"repo":"acme/app"}` {
		t.Fatalf("tool calls parsed wrong: %+v", result.ToolCalls)
	}
}

// Streaming tool_calls deltas are surfaced as StreamChunk.ToolCalls fragments.
func TestOpenAICompatClientStreamToolCalls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		frames := []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`[DONE]`,
		}
		for _, f := range frames {
			_, _ = w.Write([]byte("data: " + f + "\n\n"))
			if f == "[DONE]" {
				break
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	client := &OpenAICompatClient{}
	chunks, errCh := client.ChatStream(context.Background(),
		ProviderTarget{Name: "t", BaseURL: upstream.URL}, "cred",
		ChatRequest{Model: "stub-model", Tools: json.RawMessage(`[{"type":"function"}]`)})

	var toolDeltas []ToolCall
	finish := ""
	for chunk := range chunks {
		if len(chunk.ToolCalls) > 0 {
			toolDeltas = append(toolDeltas, chunk.ToolCalls...)
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
		if chunk.Done {
			break
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(toolDeltas) != 2 || toolDeltas[0].ID != "call_9" || toolDeltas[0].Index != 0 ||
		toolDeltas[0].Arguments != `{"q":` || toolDeltas[1].Arguments != `"x"}` {
		t.Fatalf("tool deltas parsed wrong: %+v", toolDeltas)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", finish)
	}
}

// The direct Bifrost kernel path rejects tools loudly instead of dropping them.
func TestKernelRejectsTools(t *testing.T) {
	assertInvalidArgument := func(err error, what string) {
		var e *errs.Error
		if !errors.As(err, &e) || e.Code != errs.InvalidArgument {
			t.Fatalf("%s must fail invalid_argument, got %v", what, err)
		}
	}
	k := &Kernel{}
	_, err := k.GenerateChat(context.Background(), toolTestClaims(), allowPolicy(), noopDLP(), nil,
		ChatRequest{Model: "m", Tools: json.RawMessage(`[]`)})
	assertInvalidArgument(err, "tools on kernel path")
	_, err = k.StreamChat(context.Background(), toolTestClaims(), allowPolicy(), noopDLP(),
		ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "tool", ToolCallID: "x"}}})
	assertInvalidArgument(err, "tool messages on kernel path")
}
