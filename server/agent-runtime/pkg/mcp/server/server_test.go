package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// Package-level test functions so AST parameter name discovery works.
func testAdd(a int, b int) int  { return a + b }
func testFail() (string, error) { return "", fmt.Errorf("something went wrong") }

// memTransport is an in-memory transport that uses byte buffers for testing.
type memTransport struct {
	reqBuf  *bytes.Buffer
	respBuf *bytes.Buffer
}

func newMemTransport() *memTransport {
	return &memTransport{
		reqBuf:  new(bytes.Buffer),
		respBuf: new(bytes.Buffer),
	}
}

func (t *memTransport) Start(ctx context.Context) error { return nil }
func (t *memTransport) Close() error                    { return nil }

func (t *memTransport) Send(ctx context.Context, msg any) error {
	return json.NewEncoder(t.respBuf).Encode(msg)
}

func (t *memTransport) Receive(ctx context.Context) (json.RawMessage, error) {
	line, err := t.reqBuf.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return json.RawMessage(line), nil
}

func (t *memTransport) writeRequest(method string, id any, params map[string]any) {
	req := mcp.Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	b, _ := json.Marshal(req)
	t.reqBuf.Write(b)
	t.reqBuf.WriteByte('\n')
}

func (t *memTransport) readResponse() (*mcp.Response, error) {
	line, err := t.respBuf.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp mcp.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func TestServerInitialize(t *testing.T) {
	srv := NewServer("test-server", "1.0.0")
	transport := newMemTransport()

	transport.writeRequest(mcp.MethodInitialize, "1", map[string]any{
		"protocolVersion": mcp.ProtocolVersion,
		"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
		"capabilities":    map[string]any{},
	})

	srv.processMessage(context.Background(), transport, mustRead(transport.reqBuf))

	resp, err := transport.readResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result map, got %T", resp.Result)
	}

	if v, _ := result["protocolVersion"].(string); v != mcp.ProtocolVersion {
		t.Errorf("protocolVersion = %q, want %q", v, mcp.ProtocolVersion)
	}

	serverInfo, _ := result["serverInfo"].(map[string]any)
	if name, _ := serverInfo["name"].(string); name != "test-server" {
		t.Errorf("serverInfo.name = %q, want %q", name, "test-server")
	}
}

func TestServerToolsList(t *testing.T) {
	srv := NewServer("test-server", "1.0.0")

	testTool, err := tools.NewFunction("add", "Adds two numbers", testAdd)
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	srv.RegisterTool(testTool)

	transport := newMemTransport()
	transport.writeRequest(mcp.MethodToolsList, "1", nil)
	srv.processMessage(context.Background(), transport, mustRead(transport.reqBuf))

	resp, err := transport.readResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result map")
	}

	toolsList, _ := result["tools"].([]any)
	if len(toolsList) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(toolsList))
	}

	tm := toolsList[0].(map[string]any)
	if name, _ := tm["name"].(string); name != "add" {
		t.Errorf("tool name = %q, want %q", name, "add")
	}
}

func TestServerToolsCall(t *testing.T) {
	srv := NewServer("test-server", "1.0.0")

	testTool, err := tools.NewFunction("add", "Adds two numbers", testAdd)
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	srv.RegisterTool(testTool)

	transport := newMemTransport()
	transport.writeRequest(mcp.MethodToolsCall, "1", map[string]any{
		"name":      "add",
		"arguments": map[string]any{"a": float64(3), "b": float64(4)},
	})
	srv.processMessage(context.Background(), transport, mustRead(transport.reqBuf))

	resp, err := transport.readResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result map")
	}

	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}

	cb := content[0].(map[string]any)
	if cb["type"] != "text" {
		t.Errorf("content type = %q, want %q", cb["type"], "text")
	}

	text, _ := cb["text"].(string)
	if !strings.Contains(text, "7") {
		t.Errorf("expected result containing 7, got %q", text)
	}

	isError, _ := result["isError"].(bool)
	if isError {
		t.Error("isError should be false for successful tool call")
	}
}

func TestServerToolsCallError(t *testing.T) {
	srv := NewServer("test-server", "1.0.0")

	errorTool, err := tools.NewFunction("fail", "Always fails", testFail)
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	srv.RegisterTool(errorTool)

	transport := newMemTransport()
	transport.writeRequest(mcp.MethodToolsCall, "1", map[string]any{
		"name":      "fail",
		"arguments": map[string]any{},
	})
	srv.processMessage(context.Background(), transport, mustRead(transport.reqBuf))

	resp, err := transport.readResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %s", resp.Error.Message)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result map")
	}
	isError, _ := result["isError"].(bool)
	if !isError {
		t.Error("isError should be true for failed tool call")
	}
}

func TestServerPing(t *testing.T) {
	srv := NewServer("test-server", "1.0.0")
	transport := newMemTransport()

	transport.writeRequest(mcp.MethodPing, "1", nil)
	srv.processMessage(context.Background(), transport, mustRead(transport.reqBuf))

	resp, err := transport.readResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
}

func TestServerMethodNotFound(t *testing.T) {
	srv := NewServer("test-server", "1.0.0")
	transport := newMemTransport()

	transport.writeRequest("nonexistent/method", "1", nil)
	srv.processMessage(context.Background(), transport, mustRead(transport.reqBuf))

	resp, err := transport.readResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != mcp.ErrMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, mcp.ErrMethodNotFound)
	}
}

func TestServerRegisterToolkit(t *testing.T) {
	srv := NewServer("test-server", "1.0.0")

	tk := tools.NewToolkit("test")
	t1, _ := tools.NewFunction("tool1", "First tool", testNoArgs)
	t2, _ := tools.NewFunction("tool2", "Second tool", testNoArgs2)
	tk.Add(t1)
	tk.Add(t2)

	srv.RegisterToolkit(tk)

	if len(srv.Tools()) != 2 {
		t.Errorf("expected 2 tools, got %d", len(srv.Tools()))
	}

	transport := newMemTransport()
	transport.writeRequest(mcp.MethodToolsList, "1", nil)
	srv.processMessage(context.Background(), transport, mustRead(transport.reqBuf))

	resp, _ := transport.readResponse()
	result := resp.Result.(map[string]any)
	toolsList, _ := result["tools"].([]any)
	if len(toolsList) != 2 {
		t.Errorf("tools/list returned %d tools, want 2", len(toolsList))
	}
}

func TestStdioTransport(t *testing.T) {
	var buf bytes.Buffer
	transport := NewStdioTransportRW(strings.NewReader(`{"jsonrpc":"2.0","id":"1","method":"ping"}`+"\n"), &buf)

	ctx := context.Background()
	transport.Start(ctx)

	raw, err := transport.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}

	var req mcp.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Method != "ping" {
		t.Errorf("method = %q, want ping", req.Method)
	}
}

func mustRead(buf *bytes.Buffer) json.RawMessage {
	line, err := buf.ReadBytes('\n')
	if err != nil {
		panic(err)
	}
	return json.RawMessage(line)
}

func testNoArgs() string  { return "ok" }
func testNoArgs2() string { return "ok2" }

func TestServerToolsListIncludesAnnotations(t *testing.T) {
	srv := NewServer("test-server", "1.0.0")

	testTool, err := tools.NewFunction("annotated", "Annotated tool", testNoArgs)
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	ro := true
	srv.RegisterToolWithAnnotations(testTool, &mcp.ToolAnnotations{ReadOnlyHint: &ro})

	transport := newMemTransport()
	transport.writeRequest(mcp.MethodToolsList, "1", nil)
	srv.processMessage(context.Background(), transport, mustRead(transport.reqBuf))

	resp, err := transport.readResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result map")
	}
	toolsList, ok := result["tools"].([]any)
	if !ok || len(toolsList) != 1 {
		t.Fatalf("tools = %+v, want one entry", result["tools"])
	}
	tool, ok := toolsList[0].(map[string]any)
	if !ok {
		t.Fatalf("tool entry not a map")
	}
	ann, ok := tool["annotations"].(map[string]any)
	if !ok {
		t.Fatalf("annotations missing: %+v", tool)
	}
	if got, _ := ann["readOnlyHint"].(bool); !got {
		t.Fatalf("readOnlyHint = %v, want true", ann["readOnlyHint"])
	}
}

func TestServerRegisterToolWithAnnotationsNilDelegates(t *testing.T) {
	srv := NewServer("test-server", "1.0.0")
	testTool, err := tools.NewFunction("plain", "Plain tool", testNoArgs)
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	// nil annotations must behave like RegisterTool (no wrapper, no panic).
	srv.RegisterToolWithAnnotations(testTool, nil)
	if srv.Tools() == nil || len(srv.Tools()) != 1 {
		t.Fatalf("tools = %+v, want 1", srv.Tools())
	}
}
