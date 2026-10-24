package client

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
)

// paginatedTransport serves tools/list pages by cursor for pagination tests.
type paginatedTransport struct {
	pages []map[string]any
}

func (t *paginatedTransport) Call(ctx context.Context, request any) (json.RawMessage, error) {
	b, _ := json.Marshal(request)
	var base struct {
		Method string `json:"method"`
		ID     any    `json:"id"`
		Params struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}
	_ = json.Unmarshal(b, &base)
	if base.ID == nil {
		return nil, nil
	}
	switch base.Method {
	case mcp.MethodInitialize:
		return mustMarshal(mcp.Response{JSONRPC: "2.0", ID: "test-id", Result: map[string]any{
			"protocolVersion": mcp.ProtocolVersion,
			"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
			"capabilities":    map[string]any{},
		}}), nil
	case mcp.MethodToolsList:
		pageIndex := 0
		switch base.Params.Cursor {
		case "":
			pageIndex = 0
		case "page-2":
			pageIndex = 1
		case "page-3":
			pageIndex = 2
		default:
			return nil, nil
		}
		return mustMarshal(mcp.Response{JSONRPC: "2.0", ID: "test-id", Result: t.pages[pageIndex]}), nil
	}
	return mustMarshal(mcp.Response{JSONRPC: "2.0", ID: "test-id", Result: map[string]any{}}), nil
}

func (t *paginatedTransport) Close() error { return nil }

func toolObj(name string, extra map[string]any) map[string]any {
	obj := map[string]any{"name": name, "description": "desc " + name, "inputSchema": map[string]any{"type": "object"}}
	for k, v := range extra {
		obj[k] = v
	}
	return obj
}

func TestListToolsPreservesAnnotationsAndOutputSchema(t *testing.T) {
	transport := newMemClientTransport()
	transport.setResponse(mcp.MethodInitialize, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
		"capabilities":    map[string]any{},
	})
	transport.setResponse(mcp.MethodToolsList, map[string]any{
		"tools": []any{
			toolObj("alpha", map[string]any{
				"outputSchema": map[string]any{"type": "object", "properties": map[string]any{"r": map[string]any{"type": "string"}}},
				"annotations":  map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true},
				"x-extension":  map[string]any{"vendor": "acme"},
			}),
		},
	})

	client := NewClient(transport)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	d := tools[0]
	if d.Annotations == nil || d.Annotations.ReadOnlyHint == nil || !*d.Annotations.ReadOnlyHint {
		t.Fatalf("annotations lost: %+v", d.Annotations)
	}
	if d.Annotations.DestructiveHint == nil || *d.Annotations.DestructiveHint {
		t.Fatalf("destructiveHint lost: %+v", d.Annotations.DestructiveHint)
	}
	if d.OutputSchema == nil || d.OutputSchema["type"] != "object" {
		t.Fatalf("outputSchema lost: %+v", d.OutputSchema)
	}
	if d.Metadata == nil || !strings.Contains(string(d.Metadata["x-extension"]), "acme") {
		t.Fatalf("extension metadata lost: %+v", d.Metadata)
	}
	// Round-trip must preserve metadata too.
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), "x-extension") {
		t.Fatalf("marshal dropped metadata: %s", raw)
	}
}

func TestListToolsPaginationAndStableOrder(t *testing.T) {
	transport := &paginatedTransport{pages: []map[string]any{
		{"tools": []any{toolObj("zeta", nil), toolObj("alpha", nil)}, "nextCursor": "page-2"},
		{"tools": []any{toolObj("mid", nil), toolObj("beta", nil)}, "nextCursor": "page-3"},
		{"tools": []any{toolObj("gamma", nil)}},
	}}
	client := NewClient(transport)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := []string{"alpha", "beta", "gamma", "mid", "zeta"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	for i, name := range want {
		if tools[i].Name != name {
			t.Fatalf("tools[%d] = %q, want %q (stable sort)", i, tools[i].Name, name)
		}
	}
}

func TestListToolsRejectsDuplicateName(t *testing.T) {
	transport := newMemClientTransport()
	transport.setResponse(mcp.MethodInitialize, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
		"capabilities":    map[string]any{},
	})
	transport.setResponse(mcp.MethodToolsList, map[string]any{
		"tools": []any{toolObj("dup", nil), toolObj("dup", nil)},
	})
	client := NewClient(transport)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := client.ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "duplicate tool name") {
		t.Fatalf("err = %v, want duplicate tool name", err)
	}
}

func TestListToolsRejectsCursorLoop(t *testing.T) {
	transport := &paginatedTransport{pages: []map[string]any{
		{"tools": []any{toolObj("a", nil)}, "nextCursor": "page-2"},
		{"tools": []any{toolObj("b", nil)}, "nextCursor": "page-2"}, // loops back
	}}
	client := NewClient(transport)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := client.ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "cursor loop") {
		t.Fatalf("err = %v, want cursor loop", err)
	}
}

func TestSourceCatalogRevisionChangesOnSchemaOrPolicyChange(t *testing.T) {
	base := []mcp.ToolDef{{
		Name: "alpha", Description: "d",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true)},
	}}
	rev := SourceCatalogRevisionV1(base)

	// Schema change alters the revision.
	schemaChanged := []mcp.ToolDef{{
		Name: "alpha", Description: "d",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
		OutputSchema: map[string]any{"type": "object"},
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true)},
	}}
	if got := SourceCatalogRevisionV1(schemaChanged); got == rev {
		t.Fatalf("schema change did not alter SourceCatalogRevisionV1")
	}

	// Policy (annotation) change alters the revision.
	policyChanged := []mcp.ToolDef{{
		Name: "alpha", Description: "d",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(false)},
	}}
	if got := SourceCatalogRevisionV1(policyChanged); got == rev {
		t.Fatalf("annotation change did not alter SourceCatalogRevisionV1")
	}

	// Order must not matter (unlike legacy CatalogRevision).
	reordered := []mcp.ToolDef{
		base[0],
		{Name: "beta", Description: "d", InputSchema: map[string]any{"type": "object"}},
	}
	reorderedSwapped := []mcp.ToolDef{reordered[1], reordered[0]}
	if got := SourceCatalogRevisionV1(reordered); got != SourceCatalogRevisionV1(reorderedSwapped) {
		t.Fatalf("order changed SourceCatalogRevisionV1")
	}
}

func TestSourceCatalogRevisionGoldenVector(t *testing.T) {
	defs := []mcp.ToolDef{{
		Name:        "a",
		Description: "first",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}}
	// Golden value locked on 2026-08-09; changing the canonical form or the
	// algorithm version must update this vector deliberately.
	const golden = "043d21b643646737f748181890f863b2e2fe0e5090e401bae19c417af6b6dabe"
	if got := SourceCatalogRevisionV1(defs); got != golden {
		t.Fatalf("SourceCatalogRevisionV1 = %q, want golden %q", got, golden)
	}
}

func TestDiscoverFailureKeepsPreviousCatalog(t *testing.T) {
	transport := newMemClientTransport()
	transport.setResponse(mcp.MethodInitialize, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
		"capabilities":    map[string]any{},
	})
	transport.setResponse(mcp.MethodToolsList, map[string]any{
		"tools": []any{toolObj("stable-tool", nil)},
	})

	client := NewClient(transport)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := client.DiscoverAndCache(context.Background()); err != nil {
		t.Fatalf("first discover: %v", err)
	}
	if len(client.Tools()) != 1 {
		t.Fatalf("tools after first discover = %d", len(client.Tools()))
	}

	// Server now fails; the previous snapshot must survive.
	transport.setErrorResponse(mcp.MethodToolsList, -32000, "boom")
	if err := client.DiscoverAndCache(context.Background()); err == nil {
		t.Fatalf("expected error on failing discover")
	}
	if len(client.Tools()) != 1 || client.GetTool("stable-tool") == nil {
		t.Fatalf("previous catalog was not preserved: %d tools", len(client.Tools()))
	}
}

// Notify accepts a notification without a response.
func (t *paginatedTransport) Notify(ctx context.Context, notification mcp.Notification) error {
	return nil
}
