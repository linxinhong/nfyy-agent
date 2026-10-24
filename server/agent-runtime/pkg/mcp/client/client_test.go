package client

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// memClientTransport is an in-memory transport for testing clients.
type memClientTransport struct {
	responses map[string]json.RawMessage // method → canned response
	requests  []json.RawMessage          // captured requests
}

func newMemClientTransport() *memClientTransport {
	return &memClientTransport{
		responses: make(map[string]json.RawMessage),
	}
}

func (t *memClientTransport) Call(ctx context.Context, request any) (json.RawMessage, error) {
	b, _ := json.Marshal(request)
	t.requests = append(t.requests, b)

	var base struct {
		Method string `json:"method"`
		ID     any    `json:"id"`
	}
	json.Unmarshal(b, &base)

	// Notifications (no id) are silently accepted.
	if base.ID == nil {
		return nil, nil
	}

	if resp, ok := t.responses[base.Method]; ok {
		return resp, nil
	}
	// For tests that don't care about the notification response,
	// return a default success.
	return mustMarshal(mcp.Response{JSONRPC: "2.0", ID: "test-id", Result: map[string]any{}}), nil
}

func (t *memClientTransport) Close() error { return nil }

// setResponse registers a canned response for a method.
func (t *memClientTransport) setResponse(method string, result any) {
	t.responses[method] = mustMarshal(mcp.Response{
		JSONRPC: "2.0",
		ID:      "test-id",
		Result:  result,
	})
}

func (t *memClientTransport) setErrorResponse(method string, code int, message string) {
	t.responses[method] = mustMarshal(mcp.Response{
		JSONRPC: "2.0",
		ID:      "test-id",
		Error:   &mcp.RPCError{Code: code, Message: message},
	})
}

func TestClientInitialize(t *testing.T) {
	transport := newMemClientTransport()
	transport.setResponse(mcp.MethodInitialize, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"serverInfo": map[string]any{
			"name":    "test-mcp-server",
			"version": "2.0.0",
		},
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
	})

	client := NewClient(transport)
	ctx := context.Background()
	err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}

	if client.ServerInfo().Name != "test-mcp-server" {
		t.Errorf("server name = %q, want %q", client.ServerInfo().Name, "test-mcp-server")
	}
	if client.ServerInfo().Version != "2.0.0" {
		t.Errorf("server version = %q, want %q", client.ServerInfo().Version, "2.0.0")
	}
}

func TestClientListTools(t *testing.T) {
	transport := newMemClientTransport()

	// Set initialize response.
	transport.setResponse(mcp.MethodInitialize, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
		"capabilities":    map[string]any{},
	})

	// Set tools/list response.
	transport.setResponse(mcp.MethodToolsList, map[string]any{
		"tools": []any{
			map[string]any{
				"name":        "echo",
				"description": "Echo a message",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"message": map[string]any{"type": "string"},
					},
					"required": []any{"message"},
				},
			},
			map[string]any{
				"name":        "add",
				"description": "Add two numbers",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"a": map[string]any{"type": "number"},
						"b": map[string]any{"type": "number"},
					},
				},
			},
		},
	})

	client := NewClient(transport)
	ctx := context.Background()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	// ListTools returns tools in stable (name-sorted) order.
	if tools[0].Name != "add" {
		t.Errorf("tool[0].name = %q, want %q", tools[0].Name, "add")
	}
	if tools[1].Name != "echo" {
		t.Errorf("tool[1].name = %q, want %q", tools[1].Name, "echo")
	}
}

func TestMCPToolImplementsToolInterface(t *testing.T) {
	transport := newMemClientTransport()
	transport.setResponse(mcp.MethodInitialize, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
		"capabilities":    map[string]any{},
	})
	transport.setResponse(mcp.MethodToolsCall, map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "hello world"},
		},
		"isError": false,
	})

	client := NewClient(transport)
	ctx := context.Background()
	client.Initialize(ctx)

	mcpTool := NewMCPTool(client, mcp.ToolDef{
		Name:        "echo",
		Description: "Echo back the input",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
			},
		},
	})

	// Verify it satisfies tools.Tool interface.
	var _ tools.Tool = mcpTool

	if mcpTool.Name() != "echo" {
		t.Errorf("name = %q, want %q", mcpTool.Name(), "echo")
	}

	schema := mcpTool.Schema()
	fn, ok := schema["function"].(map[string]any)
	if !ok {
		t.Fatal("schema missing function key")
	}
	if fn["name"] != "echo" {
		t.Errorf("schema name = %q, want %q", fn["name"], "echo")
	}

	// Call the tool.
	result, err := mcpTool.Call(ctx, map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result != "hello world" {
		t.Errorf("result = %q, want %q", result, "hello world")
	}
}

func TestDiscoverAndCache(t *testing.T) {
	transport := newMemClientTransport()
	transport.setResponse(mcp.MethodInitialize, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
		"capabilities":    map[string]any{},
	})
	transport.setResponse(mcp.MethodToolsList, map[string]any{
		"tools": []any{
			map[string]any{
				"name":        "tool1",
				"description": "First tool",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	})

	client := NewClient(transport)
	ctx := context.Background()
	client.Initialize(ctx)

	if err := client.DiscoverAndCache(ctx); err != nil {
		t.Fatalf("discover: %v", err)
	}

	tools := client.Tools()
	if len(tools) != 1 {
		t.Fatalf("expected 1 cached tool, got %d", len(tools))
	}

	// AsToolkit
	tk := client.AsToolkit()
	if len(tk.List()) != 1 {
		t.Errorf("toolkit has %d tools, want 1", len(tk.List()))
	}

	// AsToolList
	tl := client.AsToolList()
	if len(tl) != 1 {
		t.Errorf("tool list has %d tools, want 1", len(tl))
	}
}

func TestPool(t *testing.T) {
	transport1 := newMemClientTransport()
	transport1.setResponse(mcp.MethodInitialize, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"serverInfo":      map[string]any{"name": "server1", "version": "1.0"},
		"capabilities":    map[string]any{},
	})
	transport1.setResponse(mcp.MethodToolsList, map[string]any{
		"tools": []any{
			map[string]any{
				"name":        "tool-a",
				"description": "Tool A",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	})

	transport2 := newMemClientTransport()
	transport2.setResponse(mcp.MethodInitialize, map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"serverInfo":      map[string]any{"name": "server2", "version": "1.0"},
		"capabilities":    map[string]any{},
	})
	transport2.setResponse(mcp.MethodToolsList, map[string]any{
		"tools": []any{
			map[string]any{
				"name":        "tool-b",
				"description": "Tool B",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	})

	pool := NewPool()
	ctx := context.Background()

	_, err := pool.Add(ctx, "server1", transport1)
	if err != nil {
		t.Fatalf("add server1: %v", err)
	}
	_, err = pool.Add(ctx, "server2", transport2)
	if err != nil {
		t.Fatalf("add server2: %v", err)
	}

	tk := pool.AllTools()
	tools := tk.List()
	if len(tools) != 2 {
		t.Fatalf("expected 2 pooled tools, got %d", len(tools))
	}

	// Namespaced names should include server prefix.
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name()
	}

	hasServer1 := false
	hasServer2 := false
	for _, n := range names {
		if n == "server1::tool-a" {
			hasServer1 = true
		}
		if n == "server2::tool-b" {
			hasServer2 = true
		}
	}
	if !hasServer1 || !hasServer2 {
		t.Errorf("expected namespaced tools, got %v", names)
	}
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Notify accepts a notification without a response.
func (t *memClientTransport) Notify(ctx context.Context, notification mcp.Notification) error {
	b, _ := json.Marshal(notification)
	t.requests = append(t.requests, b)
	return nil
}
