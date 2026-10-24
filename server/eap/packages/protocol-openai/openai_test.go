package openai_test

import (
	"encoding/json"
	"strings"
	"testing"

	"enterprise-agent-platform/internal/golden"
	"enterprise-agent-platform/packages/protocol-openai"

	. "enterprise-agent-platform/packages/protocol-openai"
)

const fixtureDir = "../../contracts/schemas/v1/openai"

func TestChatCompletionRequestFixture(t *testing.T) {
	var req openai.ChatCompletionRequest
	golden.Unmarshal(t, fixtureDir+"/chat-completions.request.json", &req)
	if req.Model != "coding-high" || len(req.Messages) != 2 {
		t.Fatalf("request decoded wrong: %+v", req)
	}
	golden.Check(t, fixtureDir+"/chat-completions.request.json", req)
}

func TestChatCompletionResponseFixture(t *testing.T) {
	var resp openai.ChatCompletionResponse
	golden.Unmarshal(t, fixtureDir+"/chat-completions.response.json", &resp)
	if resp.Object != "chat.completion" || resp.Usage.TotalTokens != 192 {
		t.Fatalf("response decoded wrong: %+v", resp)
	}
	golden.Check(t, fixtureDir+"/chat-completions.response.json", resp)
}

func TestChatCompletionStreamChunkFixture(t *testing.T) {
	var chunk openai.ChatCompletionChunk
	golden.Unmarshal(t, fixtureDir+"/chat-completions.stream-chunk.json", &chunk)
	if chunk.Object != "chat.completion.chunk" {
		t.Fatalf("chunk decoded wrong: %+v", chunk)
	}
	if chunk.Choices[0].FinishReason != nil {
		t.Fatalf("intermediate chunk finish_reason must be null")
	}
	golden.Check(t, fixtureDir+"/chat-completions.stream-chunk.json", chunk)
}

func TestModelsResponseFixture(t *testing.T) {
	var models openai.ModelsResponse
	golden.Unmarshal(t, fixtureDir+"/models.response.json", &models)
	if len(models.Data) != 2 || models.Data[0].ID != "coding-high" {
		t.Fatalf("models decoded wrong: %+v", models)
	}
	golden.Check(t, fixtureDir+"/models.response.json", models)
}

func TestContractVersionFrozen(t *testing.T) {
	if openai.ContractVersion != "v1" {
		t.Fatalf("contract version changed: %s", openai.ContractVersion)
	}
}

func TestMessageUnmarshalAcceptsStringAndContentParts(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"plain"}`), &m); err != nil || m.Content != "plain" {
		t.Fatalf("string content: %v %+v", err, m)
	}
	err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"你好"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"!"}]}`), &m)
	if err != nil || m.Content != "你好!" {
		t.Fatalf("content parts must flatten text and ignore other modalities: %v %+v", err, m)
	}
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":null}`), &m); err != nil || m.Content != "" {
		t.Fatalf("null content: %v %+v", err, m)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":42}`), &m); err == nil {
		t.Fatalf("numeric content must be rejected")
	}
}

// TASK-052 / ADR-0028 decision 2: content-part arrays are preserved
// structurally (round-trip byte-faithful per part) while text parts keep
// flattening into Content for DLP.
func TestMessageContentPartsRoundTrip(t *testing.T) {
	in := `{"role":"user","content":[{"type":"text","text":"你好"},{"type":"image_url","image_url":{"url":"https://x/y.png","detail":"high"}},{"type":"text","text":"!"}]}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Content != "你好!" {
		t.Fatalf("flattened text wrong: %q", m.Content)
	}
	if len(m.ContentParts) != 3 || m.ContentParts[1].Type != "image_url" {
		t.Fatalf("parts not preserved: %+v", m.ContentParts)
	}

	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Message
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal: %v (%s)", err, out)
	}
	if back.Content != "你好!" || len(back.ContentParts) != 3 {
		t.Fatalf("structure lost in round trip: %s", out)
	}
	// The image part must be byte-identical, including fields we never typed.
	var wire struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(out, &wire); err != nil || len(wire.Content) != 3 {
		t.Fatalf("wire content must stay an array: %s", out)
	}
	if string(wire.Content[1]) != `{"type":"image_url","image_url":{"url":"https://x/y.png","detail":"high"}}` {
		t.Fatalf("non-text part not byte-faithful: %s", wire.Content[1])
	}

	// A string-content message keeps the plain string form and the
	// tool-call content-null shape.
	outStr, _ := json.Marshal(Message{Role: "user", Content: "hi"})
	if string(outStr) != `{"role":"user","content":"hi"}` {
		t.Fatalf("string form changed: %s", outStr)
	}
	outNull, _ := json.Marshal(Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1"}}})
	if !strings.Contains(string(outNull), `"content":null`) {
		t.Fatalf("tool-call content null shape changed: %s", outNull)
	}
}

// Delta with ContentParts serializes content as the parts array (streaming
// non-text deltas are forwarded structurally, never text-transformed).
func TestDeltaContentPartsMarshal(t *testing.T) {
	d := Delta{ContentParts: []ContentPart{{Type: "image_url", Raw: json.RawMessage(`{"type":"image_url","image_url":{"url":"x"}}`)}}}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"content":[{"type":"image_url","image_url":{"url":"x"}}]}` {
		t.Fatalf("delta parts shape wrong: %s", b)
	}
	b, _ = json.Marshal(Delta{Content: "hi"})
	if string(b) != `{"content":"hi"}` {
		t.Fatalf("delta string shape changed: %s", b)
	}
	b, _ = json.Marshal(Delta{})
	if string(b) != `{}` {
		t.Fatalf("empty delta must omit content: %s", b)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
