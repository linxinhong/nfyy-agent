// Package mcpgateway is the EAP governed MCP execution slice (TASK-004).
//
// Governance semantics are adapted from xingding-mcpa (ADR-0005):
//   - deny by default: a tool is visible and executable only to principals
//     whose server-validated workspace is listed on the tool;
//   - discovery and execution enforce the SAME workspace policy — a tool a
//     principal cannot execute never appears in tools/list, and a direct call
//     for a foreign tool is a recorded denial;
//   - upstream endpoints come from the registry (agents never choose URLs)
//     and connector credentials are resolved server-side by reference; they
//     are injected into the upstream request only and never returned to the
//     caller;
//   - every denial and every execution is audited through the reliable path.
package mcpgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/mcpgov"
)

// RiskLevel mirrors the PRD risk scale (R0 public read .. R5 destructive).
type RiskLevel string

const (
	RiskR0 RiskLevel = "R0"
	RiskR1 RiskLevel = "R1"
	RiskR2 RiskLevel = "R2"
	RiskR3 RiskLevel = "R3"
	RiskR4 RiskLevel = "R4"
	RiskR5 RiskLevel = "R5"
)

// RegisteredTool is one registry entry.
type RegisteredTool struct {
	Name           string
	Server         string
	Description    string
	RiskLevel      RiskLevel
	Workspaces     []string
	EndpointURL    string
	EndpointMethod string
	CredentialRef  string
}

// Registry is the persistence surface for tools. The server dimension maps to
// the PRD unified entry /mcp/{server}: a tool registered under one server is
// invisible on another.
type Registry interface {
	ListToolsForWorkspace(ctx context.Context, server, workspaceID string) ([]RegisteredTool, error)
	GetToolForWorkspace(ctx context.Context, name, server, workspaceID string) (RegisteredTool, error)
}

// CredentialResolver resolves a server-side credential reference (for example
// "env:EAP_MCP_GIT_TOKEN") to its secret at execution time. Secrets live only
// inside the execution call path.
type CredentialResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// EnvResolver resolves "env:NAME" references from the process environment.
type EnvResolver struct{}

func (EnvResolver) Resolve(_ context.Context, ref string) (string, error) {
	const prefix = "env:"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("unsupported credential reference kind: %s", ref)
	}
	name := strings.TrimPrefix(ref, prefix)
	if name == "" {
		return "", fmt.Errorf("credential reference %q has no variable name", ref)
	}
	value := osGetenv(name)
	if value == "" {
		return "", fmt.Errorf("credential %q is not configured", ref)
	}
	return value, nil
}

// osGetenv is indirect for testability.
var osGetenv = func(name string) string { return os.Getenv(name) }

// Executor performs the governed upstream call: registry-supplied endpoint,
// credential injected server-side, bounded response.
type Executor interface {
	Execute(ctx context.Context, tool RegisteredTool, args map[string]any, credential string) (json.RawMessage, error)
}

// HTTPExecutor renders the registry endpoint template with validated
// arguments and performs the upstream call. When SSRFClient is set it is
// preferred: it revalidates every redirect hop against the SSRF policy.
type HTTPExecutor struct {
	Client           *http.Client
	SSRFClient       *http.Client
	MaxResponseBytes int64
}

func (e *HTTPExecutor) client() *http.Client {
	if e.SSRFClient != nil {
		return e.SSRFClient
	}
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Execute substitutes {arg} placeholders from args (path-escaped) and calls
// the upstream endpoint with the credential as bearer — never returning it.
func (e *HTTPExecutor) Execute(ctx context.Context, tool RegisteredTool, args map[string]any, credential string) (json.RawMessage, error) {
	endpoint := tool.EndpointURL
	for key, value := range args {
		raw := fmt.Sprintf("%v", value)
		endpoint = strings.ReplaceAll(endpoint, "{"+key+"}", url.PathEscape(raw))
	}
	if strings.Contains(endpoint, "{") {
		return nil, errs.New(errs.InvalidArgument, "missing required arguments for this tool")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errs.New(errs.InvalidArgument, "tool endpoint is misconfigured")
	}

	maxBytes := e.MaxResponseBytes
	if maxBytes == 0 {
		maxBytes = 1 << 20
	}
	req, err := http.NewRequestWithContext(ctx, tool.EndpointMethod, parsed.String(), nil)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "tool request build failed")
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := e.client().Do(req)
	if err != nil {
		return nil, errs.Wrap(err, errs.UpstreamUnavailable, "tool upstream unreachable").AsRetriable()
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, errs.Wrap(err, errs.UpstreamUnavailable, "tool upstream read failed")
	}
	if int64(len(body)) > maxBytes {
		return nil, errs.New(errs.UpstreamUnavailable, "tool response exceeded the size limit")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errs.Wrap(fmt.Errorf("upstream status %d", resp.StatusCode), errs.UpstreamUnavailable, "tool upstream returned an error")
	}
	return json.RawMessage(body), nil
}

// Service is the governed MCP surface used by the gateway HTTP handler.
// Policy/Approvals/SSRF are the TASK-011 central-governance extensions; a nil
// Policy keeps the workspace-only TASK-004 semantics (dev), while a set
// Policy routes every discovery/call through the central vocabulary.
type Service struct {
	Registry  Registry
	Resolver  CredentialResolver
	Executor  Executor
	Audit     audit.Writer
	NowMillis func() int64 // optional clock override for tests
	// Policy decides on (workspace, server, tool, action); engine failure
	// denies. Nil = central policy not wired (workspace checks still apply).
	Policy mcpgov.PolicyEngine
	// Approvals stores one-time R2+ continuations. A nil store with an R2+
	// call means approval is impossible: fail closed.
	Approvals mcpgov.ApprovalStore
	// SSRF validates outbound destinations (initial + every redirect hop);
	// nil disables the check (isolated unit tests only).
	SSRF *mcpgov.SSRFPolicy
}

// ToolView is the discovery projection — registry metadata only, never
// endpoints or credentials.
type ToolView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	RiskLevel   string `json:"risk_level"`
}

// ListTools returns only the tools executable by this session's workspace on
// the requested server, filtered through the central policy when wired:
// discovery is never broader than execution authority (a centrally denied
// tool disappears from discovery too).
func (s *Service) ListTools(ctx context.Context, session identity.Session, server string) ([]ToolView, error) {
	if err := session.Validate(); err != nil {
		return nil, err
	}
	tools, err := s.Registry.ListToolsForWorkspace(ctx, server, session.Workspace.WorkspaceID)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "tool registry lookup failed")
	}
	views := make([]ToolView, 0, len(tools))
	for _, tool := range tools {
		if s.Policy != nil {
			decision, err := s.Policy.DecideMCP(ctx, session.Workspace.WorkspaceID, server, tool.Name, "mcp.discover")
			if err != nil {
				// Engine failure: discovery fails closed for the whole list.
				return nil, errs.PolicyFailClosed(err.Error())
			}
			if !decision.Allowed {
				continue // denied tools are invisible, exactly like foreign ones
			}
		}
		views = append(views, ToolView{Name: tool.Name, Description: tool.Description, RiskLevel: string(tool.RiskLevel)})
	}
	return views, nil
}

// ToolCall is one governed execution.
type ToolCall struct {
	Server        string
	Name          string
	Arguments     map[string]any
	ApprovalToken string // one-time continuation for R2+ tools
}

// ToolCallResult carries the upstream response body for the client.
type ToolCallResult struct {
	Tool   string
	Result json.RawMessage
}

// CallTool resolves, authorizes and executes one tool call. A foreign tool is
// indistinguishable from an unknown one (fail closed) and is audited as a
// denial with the same policy reason as discovery would produce.
func (s *Service) CallTool(ctx context.Context, session identity.Session, call ToolCall) (*ToolCallResult, error) {
	if err := session.Validate(); err != nil {
		return nil, err
	}
	tool, err := s.Registry.GetToolForWorkspace(ctx, call.Name, call.Server, session.Workspace.WorkspaceID)
	if err != nil {
		s.recordDenial(ctx, session, call.Name, "workspace_not_allowed")
		return nil, errs.New(errs.NotFound, "no tool visible to this workspace with that name")
	}

	// Central policy (TASK-011): engine failure denies before anything else.
	var decision mcpgov.PolicyDecision
	if s.Policy != nil {
		var derr error
		decision, derr = s.Policy.DecideMCP(ctx, session.Workspace.WorkspaceID, call.Server, call.Name, "mcp.execute")
		if derr != nil {
			s.recordDenial(ctx, session, call.Name, "policy_engine_failure")
			return nil, errs.PolicyFailClosed(derr.Error())
		}
		if !decision.Allowed {
			s.recordDenial(ctx, session, call.Name, "policy_denied:"+decision.RuleID)
			return nil, errs.New(errs.PolicyDenied, "policy denied the tool call: "+decision.Reason)
		}
	}

	// Risk/approval gate: unknown risk fails closed; R2+ never contacts the
	// upstream without consuming a valid one-time continuation.
	outcome, err := mcpgov.EvaluateApproval(string(tool.RiskLevel))
	if err != nil {
		s.recordDenial(ctx, session, call.Name, "unknown_risk")
		return nil, err
	}
	if outcome.Required {
		if s.Approvals == nil {
			s.recordDenial(ctx, session, call.Name, "approval_impossible")
			return nil, errs.New(errs.PolicyDenied, "approval required for this tool and no approval path is configured")
		}
		granted := false
		if call.ApprovalToken != "" {
			granted, err = s.Approvals.Consume(ctx, call.ApprovalToken, session.Principal.UserID, call.Name)
			if err != nil {
				return nil, errs.Wrap(err, errs.Internal, "approval check failed")
			}
		}
		if !granted {
			s.recordDenial(ctx, session, call.Name, "approval_required")
			return nil, errs.New(errs.PolicyDenied,
				fmt.Sprintf("approval required: tool %s is %s; zero upstream contact was made", call.Name, tool.RiskLevel))
		}
	}

	// SSRF: the registry endpoint must pass policy before any egress.
	if s.SSRF != nil {
		if err := s.SSRF.Check(tool.EndpointURL); err != nil {
			s.recordDenial(ctx, session, call.Name, "ssrf_blocked")
			return nil, errs.Wrap(err, errs.InvalidArgument, "upstream endpoint blocked")
		}
	}

	var credential string
	if tool.CredentialRef != "" {
		if s.Resolver == nil {
			return nil, errs.New(errs.Internal, "credential resolver is not configured")
		}
		credential, err = s.Resolver.Resolve(ctx, tool.CredentialRef)
		if err != nil {
			// A missing credential must fail the call closed, not degrade to
			// an unauthenticated request.
			return nil, errs.Wrap(err, errs.Internal, "credential resolution failed")
		}
	}

	if s.Executor == nil {
		return nil, errs.New(errs.Internal, "tool executor is not configured")
	}
	result, err := s.Executor.Execute(ctx, tool, call.Arguments, credential)
	if err != nil {
		return nil, err
	}

	ev := audit.NewEvent(audit.EventMCPToolExecuted, audit.OutcomeAllowed)
	ev.Actor = &session.Principal
	ev.WorkspaceID = session.Workspace.WorkspaceID
	ev.Resource = "mcp://" + tool.Server + "/" + tool.Name
	ev.Detail = map[string]any{"risk_level": string(tool.RiskLevel)}
	if auditErr := audit.RequireWrite(ctx, s.Audit, ev); auditErr != nil {
		// No unaudited executions: the call succeeded upstream but its result
		// is withheld unless the audit evidence landed.
		return nil, errs.New(errs.Internal, "tool execution could not be audited")
	}
	return &ToolCallResult{Tool: tool.Name, Result: result}, nil
}

func (s *Service) recordDenial(ctx context.Context, session identity.Session, toolName, reason string) {
	ev := audit.NewEvent(audit.EventMCPToolExecuted, audit.OutcomeDenied)
	ev.Actor = &session.Principal
	ev.WorkspaceID = session.Workspace.WorkspaceID
	ev.Resource = "mcp/tool/" + toolName
	ev.Detail = map[string]any{"reason": reason}
	if err := audit.RequireWrite(ctx, s.Audit, ev); err != nil {
		// Delivery failures stay observable; the denial itself stands.
		_ = err
	}
}
