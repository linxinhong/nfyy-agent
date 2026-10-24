package client

import (
	"context"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// ServerTrust declares whether an MCP server is trusted by the deployment
// (vetted) or brought by the user (byo). It narrows the conditions under
// which a tool may auto-execute without approval.
type ServerTrust string

const (
	// TrustBYO is the default: tools with readOnlyHint == true (strictly)
	// may run without approval.
	TrustBYO ServerTrust = "byo"
	// TrustVetted additionally requires destructiveHint == false and
	// idempotentHint == true before a tool may auto-execute.
	TrustVetted ServerTrust = "vetted"
)

// MCPTool wraps a remote MCP tool as a tools.Tool, allowing it to be used
// directly in AgentConfig.Tools without any changes to the agent run loop.
type MCPTool struct {
	client      *Client
	name        string
	description string
	inputSchema map[string]any
	annotations *mcp.ToolAnnotations
	trust       ServerTrust
}

// Ensure MCPTool implements tools.Tool and tools.ApprovalAware.
var (
	_ tools.Tool          = (*MCPTool)(nil)
	_ tools.ApprovalAware = (*MCPTool)(nil)
)

// NewMCPTool creates an MCPTool from an MCP tool definition.
func NewMCPTool(client *Client, def mcp.ToolDef) *MCPTool {
	t := &MCPTool{
		client:      client,
		name:        def.Name,
		description: def.Description,
		inputSchema: def.InputSchema,
		annotations: def.Annotations,
		trust:       TrustBYO,
	}
	// Normalize the input schema to always have "type": "object".
	if t.inputSchema == nil {
		t.inputSchema = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}
	if _, ok := t.inputSchema["type"]; !ok {
		t.inputSchema["type"] = "object"
	}
	return t
}

// Name returns the MCP tool name.
func (t *MCPTool) Name() string {
	return t.name
}

// Description returns the MCP tool description.
func (t *MCPTool) Description() string {
	return t.description
}

// Schema returns the tool schema in OpenAI function-calling format, which is
// what the agent model expects.
func (t *MCPTool) Schema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.name,
			"description": t.description,
			"parameters":  t.inputSchema,
		},
	}
}

// Call invokes the tool on the remote MCP server via the client.
func (t *MCPTool) Call(ctx context.Context, args map[string]any) (any, error) {
	return t.client.CallTool(ctx, t.name, args)
}

// Approval implements tools.ApprovalAware using the strict three-state
// semantics of the MCP annotations: unannotated tools and tools without an
// explicit readOnlyHint == true always require approval; only explicitly
// safe hints can downgrade to ApprovalNone. Under TrustVetted, the tool
// must additionally declare destructiveHint == false and
// idempotentHint == true.
func (t *MCPTool) Approval() tools.ApprovalKind {
	ann := t.annotations
	if ann == nil || ann.ReadOnlyHint == nil || !*ann.ReadOnlyHint {
		return tools.ApprovalRequired
	}
	if t.trust == TrustVetted {
		if ann.DestructiveHint == nil || *ann.DestructiveHint {
			return tools.ApprovalRequired
		}
		if ann.IdempotentHint == nil || !*ann.IdempotentHint {
			return tools.ApprovalRequired
		}
	}
	return tools.ApprovalNone
}

// WithServerTrust sets the trust level used by Approval and returns the
// tool for chaining. The default is TrustBYO.
func (t *MCPTool) WithServerTrust(trust ServerTrust) *MCPTool {
	t.trust = trust
	return t
}
