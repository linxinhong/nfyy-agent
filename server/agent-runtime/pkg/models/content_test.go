package models

import (
	"encoding/json"
	"testing"
)

func TestContent_MarshalText(t *testing.T) {
	c := &TextContent{Text: "hello"}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "text" {
		t.Fatalf("type = %v, want text", m["type"])
	}
	if m["text"] != "hello" {
		t.Fatalf("text = %v, want hello", m["text"])
	}
}

func TestContent_MarshalFunctionCall(t *testing.T) {
	c := &FunctionCallContent{CallID: "c1", Name: "add", Arguments: `{"a":1}`}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "function_call" {
		t.Fatalf("type = %v", m["type"])
	}
}

func TestMessage_Normalize_LegacyContent(t *testing.T) {
	msg := Message{Role: RoleUser, Content: "hello"}
	parts := msg.Normalize()
	if len(parts) != 1 {
		t.Fatalf("len = %d, want 1", len(parts))
	}
	if tc, ok := parts[0].(*TextContent); !ok || tc.Text != "hello" {
		t.Fatalf("parts[0] = %+v, want TextContent{hello}", parts[0])
	}
}

func TestMessage_Normalize_Polymorphic(t *testing.T) {
	msg := Message{
		Role: RoleAssistant,
		Contents: []Content{
			&TextContent{Text: "hi"},
			&FunctionCallContent{CallID: "c1", Name: "echo", Arguments: "{}"},
		},
	}
	parts := msg.Normalize()
	if len(parts) != 2 {
		t.Fatalf("len = %d, want 2", len(parts))
	}
	if tc, ok := parts[0].(*TextContent); !ok || tc.Text != "hi" {
		t.Fatalf("parts[0] = %+v", parts[0])
	}
	if fc, ok := parts[1].(*FunctionCallContent); !ok || fc.Name != "echo" {
		t.Fatalf("parts[1] = %+v", parts[1])
	}
}

func TestMessage_Text_Fallback(t *testing.T) {
	msg := Message{Content: "fallback"}
	if msg.Text() != "fallback" {
		t.Fatalf("Text = %q, want fallback", msg.Text())
	}
}

func TestMessage_Text_FromContents(t *testing.T) {
	msg := Message{Contents: []Content{
		&TextContent{Text: "part1"},
		&TextContent{Text: "part2"},
	}}
	if msg.Text() != "part1part2" {
		t.Fatalf("Text = %q, want part1part2", msg.Text())
	}
}

func TestContent_ImageVideoAudioFile(t *testing.T) {
	types := []Content{
		&ImageContent{URL: "http://example.com/img.png", MimeType: "image/png"},
		&AudioContent{Base64: "abc", MimeType: "audio/mp3"},
		&VideoContent{URL: "http://example.com/vid.mp4"},
		&FileContent{Name: "doc.pdf", MimeType: "application/pdf"},
	}
	for _, c := range types {
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshal %T: %v", c, err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal %T: %v", c, err)
		}
		if _, ok := m["type"]; !ok {
			t.Fatalf("%T missing type field", c)
		}
	}
}

func TestContent_Reasoning(t *testing.T) {
	c := &ReasoningContent{Text: "thinking...", ProtectedData: "secret"}
	data, _ := json.Marshal(c)
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["type"] != "reasoning" {
		t.Fatalf("type = %v", m["type"])
	}
}

func TestContent_Error(t *testing.T) {
	c := &ErrorContent{Message: "something went wrong", Code: "E001"}
	data, _ := json.Marshal(c)
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["type"] != "error" {
		t.Fatalf("type = %v", m["type"])
	}
}
