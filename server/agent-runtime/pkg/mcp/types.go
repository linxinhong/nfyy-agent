// Package mcp implements the Model Context Protocol (MCP) for
// xingding-agent-runtime. It provides both server-side exposure of tools,
// resources, and prompts, as well as client-side discovery and invocation of
// remote MCP servers.
//
// The implementation follows the JSON-RPC 2.0 transport used by MCP, with
// support for both stdio (subprocess) and Streamable HTTP transports.
package mcp

import (
	"context"
	"encoding/json"
)

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 message types
// ---------------------------------------------------------------------------

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification (no id).
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC error codes.
const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

// ---------------------------------------------------------------------------
// MCP protocol constants
// ---------------------------------------------------------------------------

// Method names defined by the MCP specification.
const (
	MethodInitialize    = "initialize"
	MethodInitialized   = "notifications/initialized"
	MethodPing          = "ping"
	MethodToolsList     = "tools/list"
	MethodToolsCall     = "tools/call"
	MethodResourcesList = "resources/list"
	MethodResourcesRead = "resources/read"
	MethodPromptsList   = "prompts/list"
	MethodPromptsGet    = "prompts/get"
)

// TransportType identifies the transport layer used by an MCP connection.
type TransportType string

const (
	TransportStdio          TransportType = "stdio"
	TransportStreamableHTTP TransportType = "streamable-http"
)

// MCP protocol version sent during initialize.
const ProtocolVersion = "2024-11-05"

// ---------------------------------------------------------------------------
// Transport interface (shared by server and client)
// ---------------------------------------------------------------------------

// Transport abstracts the underlying communication channel for JSON-RPC
// messages. Both the server and client packages consume this interface.
type Transport interface {
	// Start begins listening (server) or connecting (client).
	Start(ctx context.Context) error
	// Send writes a JSON-RPC message to the transport.
	Send(ctx context.Context, msg any) error
	// Receive reads the next JSON-RPC message from the transport.
	Receive(ctx context.Context) (json.RawMessage, error)
	// Close shuts down the transport.
	Close() error
}

// ---------------------------------------------------------------------------
// Capabilities (negotiated during initialize)
// ---------------------------------------------------------------------------

// ServerCapabilities declares what an MCP server supports.
type ServerCapabilities struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Prompts   *PromptsCapability   `json:"prompts,omitempty"`
}

// ToolsCapability advertises tool-related server features.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability advertises resource-related server features.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptsCapability advertises prompt-related server features.
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ClientCapabilities declares what an MCP client supports.
type ClientCapabilities struct {
	Roots    *RootsCapability    `json:"roots,omitempty"`
	Sampling *SamplingCapability `json:"sampling,omitempty"`
}

// RootsCapability is a placeholder for roots support.
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// SamplingCapability is a placeholder for sampling support.
type SamplingCapability struct{}

// ---------------------------------------------------------------------------
// Initialize payloads
// ---------------------------------------------------------------------------

// InitializeRequest is the params sent by the client on initialize.
type InitializeRequest struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      ImplementationInfo `json:"clientInfo"`
}

// InitializeResponse is the result returned by the server on initialize.
type InitializeResponse struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ImplementationInfo `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// ImplementationInfo identifies the client or server implementation.
type ImplementationInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ---------------------------------------------------------------------------
// Tool definitions (tools/list response)
// ---------------------------------------------------------------------------

// ToolDef describes a tool returned by tools/list.
type ToolDef struct {
	Name         string           `json:"name"`
	Description  string           `json:"description,omitempty"`
	InputSchema  map[string]any   `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	// Metadata preserves unknown extension fields verbatim so a proxying
	// client can echo them back; it is excluded from revision fingerprints
	// unless the value is a JSON primitive that can be canonicalized.
	Metadata map[string]json.RawMessage `json:"-"`
}

// MarshalJSON emits the known fields plus any preserved metadata verbatim.
func (d ToolDef) MarshalJSON() ([]byte, error) {
	type alias ToolDef
	raw, err := json.Marshal(alias(d))
	if err != nil {
		return nil, err
	}
	if len(d.Metadata) == 0 {
		return raw, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(raw, &merged); err != nil {
		return nil, err
	}
	for key, value := range d.Metadata {
		merged[key] = value
	}
	return json.Marshal(merged)
}

// ToolAnnotations carries MCP tool annotations that classify trust. Pointer
// fields distinguish "unset" (nil) from explicit false — the strict
// three-state semantics behind approval policy: only an explicit true
// grants, anything else (including unset) is treated as a denial.
type ToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
}

// ToolsListResult is the result of tools/list.
type ToolsListResult struct {
	Tools      []ToolDef `json:"tools"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// ToolsCallParams are the params sent by the client on tools/call.
type ToolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ToolsCallResult is the result of tools/call.
type ToolsCallResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock represents a piece of content in a tool result.
// MCP supports text, image, and resource content types.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// ---------------------------------------------------------------------------
// Resource definitions (resources/list and resources/read)
// ---------------------------------------------------------------------------

// Resource describes a resource exposed by the server.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourcesListResult is the result of resources/list.
type ResourcesListResult struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// ResourcesReadParams are the params for resources/read.
type ResourcesReadParams struct {
	URI string `json:"uri"`
}

// ResourcesReadResult is the result of resources/read.
type ResourcesReadResult struct {
	Contents []ResourceContent `json:"contents"`
}

// ResourceContent is a single resource content item.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ResourceHandler is the function signature for a resource provider.
type ResourceHandler func(ctx context.Context, uri string) (*ResourcesReadResult, error)

// ---------------------------------------------------------------------------
// Prompt definitions (prompts/list and prompts/get)
// ---------------------------------------------------------------------------

// Prompt describes a prompt template exposed by the server.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument is a single argument for a prompt template.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptsListResult is the result of prompts/list.
type PromptsListResult struct {
	Prompts    []Prompt `json:"prompts"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// PromptsGetParams are the params for prompts/get.
type PromptsGetParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// PromptsGetResult is the result of prompts/get.
type PromptsGetResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// PromptMessage is a single message in a prompt result.
type PromptMessage struct {
	Role    string        `json:"role"`
	Content PromptContent `json:"content"`
}

// PromptContent is the content of a prompt message.
type PromptContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// PromptHandler is the function signature for a prompt provider.
type PromptHandler func(ctx context.Context, name string, args map[string]any) (*PromptsGetResult, error)

// ---------------------------------------------------------------------------
// Helper constructors
// ---------------------------------------------------------------------------

// NewErrorResponse builds a JSON-RPC error response.
func NewErrorResponse(id any, code int, message string) Response {
	return Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: message},
	}
}

// NewSuccessResponse builds a JSON-RPC success response.
func NewSuccessResponse(id any, result any) Response {
	return Response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

// NewTextContent creates a text content block for tool results.
func NewTextContent(text string) ContentBlock {
	return ContentBlock{Type: "text", Text: text}
}
