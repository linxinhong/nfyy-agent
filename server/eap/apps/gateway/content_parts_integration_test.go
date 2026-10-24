//go:build integration

// TASK-052 / ADR-0028 decision 2: content-part arrays survive the gateway
// structurally — text parts are DLP-scanned exactly like today, non-text
// parts ride unscanned toward the provider, and provider non-text deltas are
// forwarded without entering the SSE text sanitizer.
package main_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Non-streaming: the provider receives the original content-parts array
// (decoded comparison, not string matching); the text part still carries the
// DLP rewrite; the image part is byte-identical and unscanned.
func TestContentPartStructuralPassthroughIntegration(t *testing.T) {
	var rawPayload []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawPayload, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	env := newToolEnv(t, upstream)
	key := toolEnvKey(t, env)

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key, `{
		"model":"stub-model",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe SECRET123 please"},
			{"type":"image_url","image_url":{"url":"https://example.invalid/img.png","detail":"high"}}
		]}]}`, toolAuthz(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("content-part chat: %d %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("provider payload not JSON: %v (%s)", err, rawPayload)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != "user" {
		t.Fatalf("provider messages wrong: %s", rawPayload)
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(payload.Messages[0].Content, &parts); err != nil {
		t.Fatalf("provider content must stay a parts array: %s (%s)", payload.Messages[0].Content, rawPayload)
	}
	if len(parts) != 2 {
		t.Fatalf("provider content must keep both parts: %s", rawPayload)
	}
	var textPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(parts[0], &textPart); err != nil || textPart.Type != "text" {
		t.Fatalf("text part shape wrong: %s (%v)", parts[0], err)
	}
	if textPart.Text != "describe [secret:token] please" {
		t.Fatalf("text part must keep the DLP rewrite, got: %s", textPart.Text)
	}
	const wantImage = `{"type":"image_url","image_url":{"url":"https://example.invalid/img.png","detail":"high"}}`
	if strings.TrimSpace(string(parts[1])) != wantImage {
		t.Fatalf("image part not byte-faithful: %s", parts[1])
	}
	if strings.Contains(string(rawPayload), "SECRET123") {
		t.Fatalf("raw secret must not reach the provider: %s", rawPayload)
	}
}

// Streaming: a provider delta carrying a non-text content part is emitted
// structurally and never text-transformed; the text delta still flows
// through the governed sanitizer path.
func TestContentPartStreamStructuralForwardIntegration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame := func(delta string) string {
			return "data: " + `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"stub-model","choices":[{"index":0,"delta":` + delta + `,"finish_reason":null}]}` + "\n\n"
		}
		_, _ = w.Write([]byte(
			frame(`{"role":"assistant"}`) +
				frame(`{"content":[{"type":"image_url","image_url":{"url":"https://example.invalid/img.png","detail":"high"}}]}`) +
				frame(`{"content":"SECRET123 tail"}`) +
				`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"stub-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
				"data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)
	env := newToolEnv(t, upstream)
	key := toolEnvKey(t, env)

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key, `{
		"model":"stub-model","stream":true,
		"messages":[{"role":"user","content":"hi"}]}`, toolAuthz(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("content-part stream: %d %s", rec.Code, rec.Body.String())
	}

	var sawImageDelta, sawRedactedText bool
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(line) == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content json.RawMessage `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil || len(chunk.Choices) == 0 {
			continue
		}
		content := chunk.Choices[0].Delta.Content
		if len(content) == 0 || string(content) == "null" {
			continue
		}
		var parts []json.RawMessage
		if err := json.Unmarshal(content, &parts); err == nil {
			for _, p := range parts {
				if strings.Contains(string(p), `"image_url"`) {
					if strings.TrimSpace(string(p)) != `{"type":"image_url","image_url":{"url":"https://example.invalid/img.png","detail":"high"}}` {
						t.Fatalf("streamed image part not byte-faithful: %s", p)
					}
					sawImageDelta = true
				}
			}
			continue
		}
		var text string
		if err := json.Unmarshal(content, &text); err == nil && strings.Contains(text, "[secret:token]") {
			sawRedactedText = true
		}
	}
	if !sawImageDelta {
		t.Fatalf("non-text delta must be forwarded structurally: %s", rec.Body.String())
	}
	if !sawRedactedText {
		t.Fatalf("text delta must stay on the governed sanitizer path: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET123") {
		t.Fatalf("raw secret must not reach the client stream: %s", rec.Body.String())
	}
}
