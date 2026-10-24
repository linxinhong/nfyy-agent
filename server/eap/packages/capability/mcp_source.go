package capability

import (
	"context"
	"strings"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/mcpgateway"
	"enterprise-agent-platform/packages/mcpgov"
)

// MCPSource adapts the governed MCP registry+service (TASK-004/011) into a
// capability source. Every capability keeps its risk level, workspace
// scoping and credential reference; execution delegates to the SAME
// Service.CallTool governance chain the direct-mount surface uses
// (dual-surface parity, ADR-0023).
type MCPSource struct {
	Service   *mcpgateway.Service
	Policy    mcpgov.PolicyEngine // optional policy probe for discovery status
	ListTools func(ctx context.Context, workspaceID string) ([]mcpgateway.RegisteredTool, error)
}

// Kind implements Source.
func (m *MCPSource) Kind() string { return string(KindMCP) }

func (m *MCPSource) Search(ctx context.Context, sub Subject, q SearchQuery) ([]Capability, error) {
	if m.ListTools == nil {
		return nil, nil
	}
	tools, err := m.ListTools(ctx, sub.WorkspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]Capability, 0, len(tools))
	for _, t := range tools {
		c := Capability{
			Name:        "mcp:" + t.Server + ":" + t.Name,
			Description: t.Description,
			Type:        "mcp_tool",
			RiskLevel:   string(t.RiskLevel),
			Status:      StatusAvailable,
			InputHint:   "arguments: object matching the tool contract",
		}
		if t.RiskLevel >= mcpgateway.RiskR2 {
			c.Status = StatusNeedsApproval
		}
		// Policy probe: a denied tool surfaces as denied_by_policy rather
		// than invisible — agents plan better on explicit denials.
		if m.Policy != nil {
			if d, err := m.Policy.DecideMCP(ctx, sub.WorkspaceID, t.Server, t.Name, "mcp.execute"); err == nil && !d.Allowed {
				c.Status = StatusDeniedByPolicy
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (m *MCPSource) Parse(ctx context.Context, sub Subject, name string) (*Parsed, error) {
	server, tool, ok := splitMCPName(name)
	if !ok {
		return nil, nil
	}
	t, err := m.Service.Registry.GetToolForWorkspace(ctx, tool, server, sub.WorkspaceID)
	if err != nil {
		return nil, nil // not ours (or invisible) — let the next source try
	}
	return &Parsed{Source: KindMCP, Name: name, Risk: string(t.RiskLevel)}, nil
}

func (m *MCPSource) Execute(ctx context.Context, sub Subject, parsed *Parsed, in ExecuteInput) (ExecuteResult, error) {
	server, tool, ok := splitMCPName(parsed.Name)
	if !ok {
		return ExecuteResult{}, ErrUnknown(parsed.Name)
	}
	session := identity.Session{
		Principal: identity.Principal{UserID: sub.UserID},
		Workspace: identity.WorkspaceContext{
			WorkspaceID: sub.WorkspaceID, Roles: sub.Roles, Source: identity.WorkspaceSourceMembership,
		},
	}
	res, err := m.Service.CallTool(ctx, session, mcpgateway.ToolCall{
		Server:        server,
		Name:          tool,
		Arguments:     in.Args,
		ApprovalToken: in.ApprovalToken,
	})
	if err != nil {
		return ExecuteResult{}, translateMCPError(err)
	}
	return ExecuteResult{Content: string(res.Result), RiskLevel: parsed.Risk, Kind: "mcp_tool"}, nil
}

func splitMCPName(name string) (server, tool string, ok bool) {
	parts := strings.SplitN(name, ":", 3)
	if len(parts) != 3 || parts[0] != "mcp" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// translateMCPError maps the frozen EAP problem codes onto the capability
// error taxonomy with retry guidance.
func translateMCPError(err error) error {
	if p, isEAP := errs.From(err); isEAP {
		_ = p
		switch p.Code {
		case errs.PolicyDenied:
			if containsAny(p.Message, "approval required") {
				return &Error{Code: "needs_approval", Message: p.Message,
					RetryAction: "request_approval", ApprovalHint: "obtain a one-time approval continuation and retry"}
			}
			return &Error{Code: "policy_blocked", Message: p.Message, RetryAction: "contact_admin"}
		case errs.NotFound:
			return ErrUnknown("")
		case errs.Unauthenticated:
			return &Error{Code: "forbidden", Message: p.Message}
		case errs.InvalidArgument:
			return &Error{Code: "invalid_arguments", Message: p.Message, RetryAction: "correct_arguments"}
		}
	}
	return &Error{Code: "internal", Message: err.Error()}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
