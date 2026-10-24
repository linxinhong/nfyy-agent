package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
)

// RC-12 regression tests: the client must prefer structuredContent when the
// server ships it (some servers truncate text to ≤2KB), and keep the exact
// v0.2.1 text behavior otherwise.

func initResult(t *testing.T, result map[string]any) (*Client, *memClientTransport) {
	t.Helper()
	tr := newMemClientTransport()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	tr.responses[mcp.MethodToolsCall] = b
	c := NewClient(tr)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, tr
}

// 1+3. structuredContent present (with text alongside) → returned
// preferentially, >2KB payloads arrive intact.
func TestCallToolPrefersStructuredContent(t *testing.T) {
	big := make(map[string]any)
	bigRows := make([]any, 64)
	for i := range bigRows {
		bigRows[i] = map[string]any{"row": strings.Repeat("x", 100), "seq": i}
	}
	big["rows"] = bigRows
	payload, _ := json.Marshal(big)
	if len(payload) <= 2048 {
		t.Fatalf("fixture must exceed 2KB, got %d", len(payload))
	}
	c, _ := initResult(t, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": `{"truncated": true}`}},
		"structuredContent": big,
	})
	out, err := c.CallTool(context.Background(), "tool", nil)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map from structuredContent, got %T", out)
	}
	rows, _ := m["rows"].([]any)
	if len(rows) != 64 {
		t.Fatalf("structured payload truncated: %d rows", len(rows))
	}
}

// 2. structuredContent missing or null → identical v0.2.1 behavior (text
// concatenation).
func TestCallToolTextFallbackUnchanged(t *testing.T) {
	for _, result := range []map[string]any{
		{"content": []any{map[string]any{"type": "text", "text": `{"a":1}`}}},
		{"content": []any{map[string]any{"type": "text", "text": `{"a":1}`}}, "structuredContent": nil},
	} {
		c, _ := initResult(t, result)
		out, err := c.CallTool(context.Background(), "tool", nil)
		if err != nil {
			t.Fatal(err)
		}
		s, ok := out.(string)
		if !ok || s != `{"a":1}` {
			t.Fatalf("text fallback must be unchanged, got %#v", out)
		}
	}
}

// 4. isError path keeps returning an error built from text (no regression,
// structured preference never applies to errors).
func TestCallToolIsErrorUnchanged(t *testing.T) {
	c, _ := initResult(t, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": "boom"}},
		"isError":           true,
		"structuredContent": map[string]any{"ignored": true},
	})
	if _, err := c.CallTool(context.Background(), "tool", nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("isError path regressed: %v", err)
	}
}

// 5. MCPTool.Call surfaces the structured result unchanged (Pool prefix path
// composes the same CallTool).
func TestMCPToolCallStructuredFidelity(t *testing.T) {
	structured := map[string]any{"alerts": []any{map[string]any{"id": "a1"}, map[string]any{"id": "a2"}}}
	c, _ := initResult(t, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": `{"alerts":[]}`}},
		"structuredContent": structured,
	})
	tool := NewMCPTool(c, mcp.ToolDef{Name: "tool", Description: "test tool"})
	out, err := tool.Call(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("MCPTool.Call must pass structured result through, got %T", out)
	}
	if alerts, _ := m["alerts"].([]any); len(alerts) != 2 {
		t.Fatalf("structured result mutated: %+v", m)
	}
}
