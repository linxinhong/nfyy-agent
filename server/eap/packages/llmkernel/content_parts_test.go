package llmkernel

// TASK-052 / ADR-0028 decision 2: content-part arrays survive structurally
// toward the provider while the flattened text drives DLP exactly as the
// plain-string path does, and redactions are written back into the text
// parts of the preserved structure.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type partsFakeDLP struct{ replaced string }

func (f partsFakeDLP) Scan(_ context.Context, text string) (DLPResult, error) {
	if strings.Contains(text, "SECRET123") {
		return DLPResult{Redacted: strings.ReplaceAll(text, "SECRET123", f.replaced), RedactionCount: strings.Count(text, "SECRET123"), RuleNames: []string{"secret-token"}}, nil
	}
	return DLPResult{Redacted: text}, nil
}

func partsRequest(t *testing.T, textPart string) ChatRequest {
	t.Helper()
	part := func(s string) string {
		b, err := json.Marshal(map[string]any{"type": "text", "text": s})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	imagePart := `{"type":"image_url","image_url":{"url":"https://x/y.png"}}`
	return ChatRequest{Model: "m", Messages: []ChatMessage{{
		Role:    "user",
		Content: textPart,
		ContentParts: []ContentPart{
			{Type: "text", Text: textPart, Raw: json.RawMessage(part(textPart))},
			{Type: "image_url", Raw: json.RawMessage(imagePart)},
		},
	}}}
}

// The flattened text path is unchanged (byte-equivalent scan input) while
// the preserved structure keeps its shape; redaction rewrites text parts in
// place and never touches non-text parts.
func TestScanMessagesRedactsTextPartsStructurally(t *testing.T) {
	req := partsRequest(t, "note SECRET123 here")
	res, err := scanMessages(context.Background(), partsFakeDLP{replaced: "[secret:token]"}, req, ScanEnforce)
	if err != nil || res.RedactionCount != 1 {
		t.Fatalf("scan: %v %+v", err, res)
	}
	m := req.Messages[0]
	if m.Content != "note [secret:token] here" {
		t.Fatalf("flattened content not redacted: %q", m.Content)
	}
	if len(m.ContentParts) != 2 {
		t.Fatalf("structure lost: %d parts", len(m.ContentParts))
	}
	var textPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.ContentParts[0].Raw, &textPart); err != nil || textPart.Text != "note [secret:token] here" {
		t.Fatalf("text part not rewritten: %s %v", m.ContentParts[0].Raw, err)
	}
	if string(m.ContentParts[1].Raw) != `{"type":"image_url","image_url":{"url":"https://x/y.png"}}` {
		t.Fatalf("non-text part must ride untouched: %s", m.ContentParts[1].Raw)
	}
}

// When nothing is redacted the preserved parts pass through byte-faithfully.
func TestScanMessagesCleanPartsPassThrough(t *testing.T) {
	req := partsRequest(t, "clean text")
	if _, err := scanMessages(context.Background(), partsFakeDLP{replaced: "X"}, req, ScanEnforce); err != nil {
		t.Fatal(err)
	}
	m := req.Messages[0]
	if m.Content != "clean text" || m.ContentParts[0].Text != "clean text" {
		t.Fatalf("clean content mutated: %+v", m)
	}
	if string(m.ContentParts[1].Raw) != `{"type":"image_url","image_url":{"url":"https://x/y.png"}}` {
		t.Fatalf("non-text part mutated: %s", m.ContentParts[1].Raw)
	}
}

// The provider payload carries the original array, not the flattened string.
func TestOpenAIPayloadForwardsPartsStructurally(t *testing.T) {
	req := partsRequest(t, "note SECRET123 here")
	if _, err := scanMessages(context.Background(), partsFakeDLP{replaced: "[secret:token]"}, req, ScanEnforce); err != nil {
		t.Fatal(err)
	}
	payload := openAIChatPayload{Model: req.Model}
	for _, m := range req.Messages {
		content := any(m.Content)
		if parts := contentPartsWire(m.ContentParts); parts != nil {
			content = parts
		}
		payload.Messages = append(payload.Messages, openAIMessageIn{Role: m.Role, Content: content})
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("content must stay an array on the wire: %s", b)
	}
	if len(wire.Messages) != 1 || len(wire.Messages[0].Content) != 2 {
		t.Fatalf("parts not forwarded structurally: %s", b)
	}
	if !strings.Contains(string(wire.Messages[0].Content[0]), "[secret:token]") ||
		strings.Contains(string(b), "SECRET123") {
		t.Fatalf("text part must carry the redacted text: %s", b)
	}
	if string(wire.Messages[0].Content[1]) != `{"type":"image_url","image_url":{"url":"https://x/y.png"}}` {
		t.Fatalf("image part not byte-faithful: %s", wire.Messages[0].Content[1])
	}
}

// Provider replies and deltas that arrive as parts arrays are decoded into
// flattened text plus preserved structure (decodeReplyContent).
func TestDecodeReplyContentParts(t *testing.T) {
	text, parts := decodeReplyContent(json.RawMessage(`"hi"`))
	if text != "hi" || parts != nil {
		t.Fatalf("string reply: %q %+v", text, parts)
	}
	text, parts = decodeReplyContent(json.RawMessage(`[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}}]`))
	if text != "a" || len(parts) != 2 || parts[1].Type != "image_url" {
		t.Fatalf("parts reply: %q %+v", text, parts)
	}
	if string(parts[1].Raw) != `{"type":"image_url","image_url":{"url":"x"}}` {
		t.Fatalf("part raw lost: %s", parts[1].Raw)
	}
	text, parts = decodeReplyContent(json.RawMessage(`null`))
	if text != "" || parts != nil {
		t.Fatalf("null reply: %q %+v", text, parts)
	}
}
