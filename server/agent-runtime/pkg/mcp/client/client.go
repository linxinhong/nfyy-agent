// Package client implements an MCP (Model Context Protocol) client that
// connects to MCP servers, discovers their tools, and exposes them as
// pkg/tools.Tool instances that can be used directly by agents.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
)

// ClientTransport is the transport abstraction used by Client. Unlike the
// shared mcp.Transport, this is specifically for the client side and uses
// a single Call method for request-response.
type ClientTransport interface {
	// Call sends a JSON-RPC request and returns the raw response.
	Call(ctx context.Context, request any) (json.RawMessage, error)
	// Notify sends a JSON-RPC notification (no id, no response expected).
	Notify(ctx context.Context, notification mcp.Notification) error
	// Close shuts down the transport.
	Close() error
}

// Pagination and size limits for tools/list. The defaults can be lowered,
// or raised up to the explicit hard caps, via SetListToolsLimits.
const (
	defaultListToolsMaxPages = 100
	defaultListToolsMaxTools = 10_000
	hardListToolsMaxPages    = 1_000
	hardListToolsMaxTools    = 100_000
)

// Client manages a connection to an MCP server.
type Client struct {
	transport    ClientTransport
	serverInfo   mcp.ImplementationInfo
	capabilities mcp.ServerCapabilities
	instructions string

	mu                sync.RWMutex
	tools             map[string]*MCPTool
	listToolsMaxPages int
	listToolsMaxTools int

	closed atomic.Bool
	nextID atomic.Int64

	// Session recovery (E2E-04 deviation #1): a server restart invalidates
	// the negotiated session (HTTP 404). recoverMu serializes one-flight
	// re-initialization; sessionGen lets concurrent callers skip a redundant
	// handshake when someone else already recovered.
	recoverMu  sync.Mutex
	sessionGen int64
}

// NewClient creates a client over the given transport.
func NewClient(transport ClientTransport) *Client {
	return &Client{
		transport:         transport,
		tools:             make(map[string]*MCPTool),
		listToolsMaxPages: defaultListToolsMaxPages,
		listToolsMaxTools: defaultListToolsMaxTools,
	}
}

// SetListToolsLimits adjusts the tools/list pagination caps. Values must be
// positive and may not exceed the hard caps; the defaults can only be
// lowered, or raised within the hard caps.
func (c *Client) SetListToolsLimits(maxPages, maxTools int) error {
	if maxPages <= 0 || maxPages > hardListToolsMaxPages || maxTools <= 0 || maxTools > hardListToolsMaxTools {
		return fmt.Errorf("mcp client: list tools limits out of range (pages 1-%d, tools 1-%d)", hardListToolsMaxPages, hardListToolsMaxTools)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listToolsMaxPages = maxPages
	c.listToolsMaxTools = maxTools
	return nil
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize(ctx context.Context) error {
	requestedVersion := mcp.ProtocolVersion
	if ht, ok := c.transport.(*HTTPClientTransport); ok && ht.protocolVersion() != "" {
		requestedVersion = ht.protocolVersion()
	}
	params := mcp.InitializeRequest{
		ProtocolVersion: requestedVersion,
		Capabilities:    mcp.ClientCapabilities{},
		ClientInfo: mcp.ImplementationInfo{
			Name:    "xingding-agent-runtime",
			Version: "0.1.0",
		},
	}

	resp, err := c.callRetryable(ctx, mcp.MethodInitialize, toMap(params), false)
	if err != nil {
		return fmt.Errorf("mcp client: initialize: %w", err)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		return fmt.Errorf("mcp client: invalid initialize response")
	}

	if info, ok := result["serverInfo"].(map[string]any); ok {
		c.serverInfo.Name = strVal(info, "name")
		c.serverInfo.Version = strVal(info, "version")
	}
	c.capabilities = parseServerCapabilities(mapVal(result, "capabilities"))
	c.instructions = strVal(result, "instructions")

	// Send initialized notification.
	notification := mcp.Notification{
		JSONRPC: "2.0",
		Method:  mcp.MethodInitialized,
	}
	if err := c.notify(ctx, notification); err != nil {
		return fmt.Errorf("mcp client: send initialized: %w", err)
	}

	return nil
}

// CallTool invokes a tool on the remote MCP server.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (any, error) {
	if args == nil {
		args = map[string]any{}
	}
	params := mcp.ToolsCallParams{
		Name:      name,
		Arguments: args,
	}
	resp, err := c.call(ctx, mcp.MethodToolsCall, toMap(params))
	if err != nil {
		return nil, fmt.Errorf("mcp client: call tool %q: %w", name, err)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mcp client: invalid tools/call response")
	}

	content, _ := result["content"].([]any)
	isError, _ := result["isError"].(bool)

	if isError {
		return nil, fmt.Errorf("mcp tool %q error: %v", name, extractText(content))
	}

	// RC-12: prefer the server's structuredContent when present and non-null.
	// Some servers (e.g. logaduit) truncate text content to a size cap while
	// shipping the full result in structuredContent; returning the text in
	// that case silently truncated tool results for callers. When absent or
	// null the v0.2.1 text-concatenation behavior is preserved unchanged.
	if structured, present := result["structuredContent"]; present && structured != nil {
		if sm, ok := structured.(map[string]any); ok {
			return sm, nil
		}
	}

	return extractText(content), nil
}

// ListTools fetches the server's tool list.
func (c *Client) ListTools(ctx context.Context) ([]mcp.ToolDef, error) {
	c.mu.RLock()
	maxPages, maxTools := c.listToolsMaxPages, c.listToolsMaxTools
	c.mu.RUnlock()

	var all []mcp.ToolDef
	seen := make(map[string]struct{})
	seenCursors := make(map[string]struct{})
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("mcp client: tools/list exceeded %d pages", maxPages)
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		resp, err := c.call(ctx, mcp.MethodToolsList, params)
		if err != nil {
			return nil, fmt.Errorf("mcp client: list tools: %w", err)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mcp client: invalid tools/list response")
		}
		toolsRaw, _ := result["tools"].([]any)
		defs, err := decodeToolDefs(toolsRaw)
		if err != nil {
			return nil, err
		}
		for _, d := range defs {
			if _, dup := seen[d.Name]; dup {
				return nil, fmt.Errorf("mcp client: duplicate tool name %q", d.Name)
			}
			seen[d.Name] = struct{}{}
			all = append(all, d)
		}
		if len(all) > maxTools {
			return nil, fmt.Errorf("mcp client: tools/list exceeded %d tools", maxTools)
		}
		next, _ := result["nextCursor"].(string)
		if next == "" {
			break
		}
		if _, loop := seenCursors[next]; loop {
			return nil, fmt.Errorf("mcp client: tools/list cursor loop at %q", next)
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}
	// Stable ordering so catalog snapshots never drift by map iteration.
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

// decodeToolDefs typed-decodes a raw tools/list page, preserving
// annotations, output schema, and any unknown extension fields verbatim.
func decodeToolDefs(toolsRaw []any) ([]mcp.ToolDef, error) {
	raw, err := json.Marshal(toolsRaw)
	if err != nil {
		return nil, fmt.Errorf("mcp client: encode tool page: %w", err)
	}
	var defs []mcp.ToolDef
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil, fmt.Errorf("mcp client: invalid tool definition: %w", err)
	}
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &objects); err != nil {
		return nil, fmt.Errorf("mcp client: invalid tool definition: %w", err)
	}
	for i := range defs {
		metadata := make(map[string]json.RawMessage)
		for key, value := range objects[i] {
			switch key {
			case "name", "description", "inputSchema", "outputSchema", "annotations":
			default:
				metadata[key] = value
			}
		}
		if len(metadata) > 0 {
			defs[i].Metadata = metadata
		}
	}
	return defs, nil
}

// ServerInfo returns the server metadata.
func (c *Client) ServerInfo() mcp.ImplementationInfo { return c.serverInfo }

// Capabilities returns the server's declared capabilities.
func (c *Client) Capabilities() mcp.ServerCapabilities { return c.capabilities }

// Close shuts down the client and its transport.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.transport.Close()
}

// call sends a JSON-RPC request and waits for the response.
func (c *Client) call(ctx context.Context, method string, params map[string]any) (*mcp.Response, error) {
	return c.callRetryable(ctx, method, params, true)
}

// callRetryable performs one JSON-RPC call. When allowRecovery is set and the
// transport reports a session-invalid response (HTTP 404 — the server forgot
// the session, typically after a gateway restart), the client re-initializes
// once (single-flight) and retries the original request. MCP requests are
// idempotent from the client's perspective here: secops tool calls carry
// server-side idempotency keys, and a duplicate initialize is harmless.
func (c *Client) callRetryable(ctx context.Context, method string, params map[string]any, allowRecovery bool) (*mcp.Response, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("mcp client: closed")
	}
	gen := c.currentSessionGen()
	resp, err := c.doCall(ctx, method, params)
	if err == nil || !allowRecovery || !isSessionInvalidError(err) {
		return resp, err
	}

	c.recoverMu.Lock()
	recovered := false
	if c.currentSessionGen() == gen { // nobody else recovered yet
		if initErr := c.reinitialize(ctx); initErr != nil {
			c.recoverMu.Unlock()
			return nil, fmt.Errorf("session recovery failed after 404: %w (original: %v)", initErr, err)
		}
		c.sessionGen++
		recovered = true
	}
	c.recoverMu.Unlock()
	_ = recovered // concurrent callers share the recovery via generation bump

	retry, retryErr := c.doCall(ctx, method, params)
	if retryErr != nil {
		return retry, fmt.Errorf("after session recovery: %w (original: %v)", retryErr, err)
	}
	return retry, nil
}

func (c *Client) doCall(ctx context.Context, method string, params map[string]any) (*mcp.Response, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("mcp client: closed")
	}

	id := fmt.Sprintf("req-%d", c.nextID.Add(1))
	req := mcp.Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	raw, err := c.transport.Call(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("transport call: %w", err)
	}

	var resp mcp.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error [%d]: %s", resp.Error.Code, resp.Error.Message)
	}
	return &resp, nil
}

func (c *Client) currentSessionGen() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionGen
}

// isSessionInvalidError reports whether err is an HTTP 404 from the transport
// (MCP streamable-HTTP servers answer unknown/expired sessions with 404).
func isSessionInvalidError(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound
}

// reinitialize performs a fresh MCP handshake (new session) on the existing
// transport. Caller must hold recoverMu.
func (c *Client) reinitialize(ctx context.Context) error {
	// Initialize captures the new session id inside the transport; the
	// initialized notification completes the handshake.
	return c.Initialize(ctx)
}

// notify sends a JSON-RPC notification (no response expected).
func (c *Client) notify(ctx context.Context, notification mcp.Notification) error {
	return c.transport.Notify(ctx, notification)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func parseServerCapabilities(caps map[string]any) mcp.ServerCapabilities {
	sc := mcp.ServerCapabilities{}
	if tools, ok := caps["tools"].(map[string]any); ok {
		sc.Tools = &mcp.ToolsCapability{}
		sc.Tools.ListChanged, _ = tools["listChanged"].(bool)
	}
	if resources, ok := caps["resources"].(map[string]any); ok {
		sc.Resources = &mcp.ResourcesCapability{}
		sc.Resources.ListChanged, _ = resources["listChanged"].(bool)
	}
	if prompts, ok := caps["prompts"].(map[string]any); ok {
		sc.Prompts = &mcp.PromptsCapability{}
		sc.Prompts.ListChanged, _ = prompts["listChanged"].(bool)
	}
	return sc
}

func extractText(content []any) string {
	if len(content) == 0 {
		return ""
	}
	var parts []string
	for _, c := range content {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cm["type"].(string); t == "text" {
			if text, _ := cm["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	b, _ := json.Marshal(parts)
	return string(b)
}

func strVal(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func mapVal(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}
