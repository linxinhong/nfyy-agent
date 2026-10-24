// Package mcp defines the EAP MCP wire contract (v1): the JSON-RPC 2.0
// envelope and the initialize / tools-list messages carried over Streamable
// HTTP. Shapes are frozen by golden fixtures under contracts/schemas/v1/mcp/.
package mcp

// ContractVersion identifies the frozen contract generation.
const ContractVersion = "v1"

// ProtocolVersion is the MCP protocol version this contract speaks.
const ProtocolVersion = "2025-06-18"

// Standard JSON-RPC error codes used by the gateway.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Implementation names a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// RootsCapability advertises roots support.
type RootsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// ToolsCapability advertises tools support.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// ClientCapabilities is the client feature advertisement.
type ClientCapabilities struct {
	Roots *RootsCapability `json:"roots,omitempty"`
}

// ServerCapabilities is the server feature advertisement.
type ServerCapabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
	Roots *RootsCapability `json:"roots,omitempty"`
}

// InitializeParams is the initialize request payload.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the initialize response payload.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
}

// InitializeRequest is the JSON-RPC initialize call.
type InitializeRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      any              `json:"id"`
	Method  string           `json:"method"`
	Params  InitializeParams `json:"params"`
}

// InitializeResponse is the JSON-RPC initialize result.
type InitializeResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      any              `json:"id"`
	Result  InitializeResult `json:"result"`
}

// ToolsListRequest is the JSON-RPC tools/list call.
type ToolsListRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Method  string `json:"method"`
}

// JSONSchema is the JSON-Schema fragment describing a tool's input. Only the
// fields the V1 registry needs are typed; the rest stay open via Properties.
type JSONSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
	Required   []string       `json:"required,omitempty"`
}

// Tool is one registered tool as returned by tools/list.
type Tool struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	InputSchema JSONSchema `json:"inputSchema"`
}

// ToolsListResult is the tools/list payload.
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

// ToolsListResponse is the JSON-RPC tools/list result.
type ToolsListResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  ToolsListResult `json:"result"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse is the JSON-RPC error reply.
type ErrorResponse struct {
	JSONRPC string   `json:"jsonrpc"`
	ID      any      `json:"id"`
	Error   RPCError `json:"error"`
}

// ToolCallParams is the tools/call request payload. ApprovalToken carries a
// one-time approved continuation for R2+ tools.
type ToolCallParams struct {
	Name          string         `json:"name"`
	Arguments     map[string]any `json:"arguments,omitempty"`
	ApprovalToken string         `json:"approval_token,omitempty"`
}

// TextContent is one text content block of a tool result.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolCallResult is the tools/call result payload.
type ToolCallResult struct {
	Content []TextContent `json:"content"`
	IsError bool          `json:"is_error,omitempty"`
}

// ToolCallRequest is the JSON-RPC tools/call call.
type ToolCallRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Method  string         `json:"method"`
	Params  ToolCallParams `json:"params"`
}

// ToolCallResponse is the JSON-RPC tools/call reply.
type ToolCallResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Result  ToolCallResult `json:"result"`
}
