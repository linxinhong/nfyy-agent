package main

// /mcp/agent — the Capability Gateway meta-tool surface (TASK-034,
// ADR-0023). Exactly two tools regardless of capability count:
// search_capabilities (permission-aware discovery) and execute_capability
// (full governance chain with DLP egress + reliable audit). The direct-mount
// /mcp/{server} surface stays; both share the same governance.

import (
	"encoding/json"
	"net/http"
	"strings"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/capability"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/protocol-mcp"
)

func (s *Server) handleAgentMCP(w http.ResponseWriter, r *http.Request) {
	if s.Capabilities == nil || s.MCP == nil {
		writeProblem(w, r, errs.New(errs.NotFound, "capability gateway is not configured"))
		return
	}
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	r = withDLPSubject(r, session.Workspace.WorkspaceID, session.Workspace.Roles, false)

	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := decodeJSONBody(w, r, &envelope); err != nil {
		writeProblem(w, r, err)
		return
	}

	switch envelope.Method {
	case "initialize":
		if sid := mcpSessionID(); sid != "" {
			w.Header().Set("Mcp-Session-Id", sid)
		}
		writeMCPResult(w, r, envelope.ID, mcp.InitializeResult{
			ProtocolVersion: mcp.ProtocolVersion,
			Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{ListChanged: false}},
			ServerInfo:      mcp.Implementation{Name: "eap-capability-gateway", Version: Version},
		})
	case "tools/list":
		// Explicit schemas: agents cannot guess the argument shape from an
		// empty object schema, and every failed guess costs a round trip.
		tools := []mcp.Tool{
			{
				Name:        "search_capabilities",
				Description: "Find enterprise capabilities you may use (MCP tools, Skills). Results are filtered by your workspace, policy and risk posture; status explains availability. Call with an empty query to list everything available to you.",
				InputSchema: mcp.JSONSchema{
					Type: "object",
					Properties: map[string]any{
						"query": map[string]any{"type": "string", "description": "Keywords to search for; omit or leave empty to list all capabilities available to you."},
						"limit": map[string]any{"type": "integer", "description": "Maximum number of matches (default 5, max 20)."},
					},
				},
			},
			{
				Name:        "execute_capability",
				Description: "Execute a capability by its exact name from search_capabilities (mcp:<server>:<tool> or skill:<name>[@version]). High-risk (R2+) capabilities require an approval continuation.",
				InputSchema: mcp.JSONSchema{
					Type:     "object",
					Required: []string{"name"},
					Properties: map[string]any{
						"name":           map[string]any{"type": "string", "description": "Exact capability name from search_capabilities, e.g. mcp:gitlab:git_read_file"},
						"arguments":      map[string]any{"type": "object", "description": "The capability's tool arguments object, e.g. {\"repo\":\"acme/app\",\"path\":\"README.md\"}"},
						"approval_token": map[string]any{"type": "string", "description": "One-time approval continuation, required only for needs_approval capabilities."},
					},
				},
			},
		}
		writeMCPResult(w, r, envelope.ID, mcp.ToolsListResult{Tools: tools})
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if len(envelope.Params) > 0 {
			if err := json.Unmarshal(envelope.Params, &params); err != nil {
				writeMCPRPCError(w, r, envelope.ID, mcp.CodeInvalidParams, "tools/call params must be an object")
				return
			}
		}
		sub := capability.Subject{
			UserID:      session.Principal.UserID,
			WorkspaceID: session.Workspace.WorkspaceID,
			Roles:       session.Workspace.Roles,
		}
		switch params.Name {
		case "search_capabilities":
			s.agentSearch(w, r, envelope.ID, sub, params.Arguments)
		case "execute_capability":
			s.agentExecute(w, r, envelope.ID, session, sub, params.Arguments)
		default:
			writeMCPRPCError(w, r, envelope.ID, mcp.CodeMethodNotFound,
				"this surface exposes only search_capabilities and execute_capability")
		}
	default:
		writeMCPRPCError(w, r, envelope.ID, mcp.CodeMethodNotFound, "method not supported by this server")
	}
}

func (s *Server) agentSearch(w http.ResponseWriter, r *http.Request, id json.RawMessage, sub capability.Subject, args map[string]any) {
	query := ""
	if q, ok := args["query"].(string); ok {
		query = q
	}
	limit := 5
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	matches, err := s.Capabilities.Search(r.Context(), sub, capability.SearchQuery{Text: query, Limit: limit})
	if err != nil {
		writeMCPRPCError(w, r, id, mcp.CodeInternalError, rpcMessage(err))
		return
	}
	payload := map[string]any{"matches": matches}
	if len(matches) == 0 {
		payload["hint"] = "no capability matched; try broader keywords"
	}
	b, _ := json.Marshal(payload)
	writeMCPResult(w, r, id, mcp.ToolCallResult{
		Content: []mcp.TextContent{{Type: "text", Text: string(b)}},
	})
}

func (s *Server) agentExecute(w http.ResponseWriter, r *http.Request, id json.RawMessage, session identity.Session, sub capability.Subject, args map[string]any) {
	name, _ := args["name"].(string)
	if name == "" {
		writeMCPResult(w, r, id, mcp.ToolCallResult{
			IsError: true,
			Content: []mcp.TextContent{{Type: "text", Text: capabilityErrorJSON(&capability.Error{Code: "invalid_arguments", Message: `name is required: the tool takes {"name":"mcp:<server>:<tool>","arguments":{...}} — get exact names from search_capabilities`, RetryAction: "correct_arguments"})}},
		})
		return
	}
	toolArgs, _ := args["arguments"].(map[string]any)
	approval, _ := args["approval_token"].(string)

	res, err := s.Capabilities.Execute(r.Context(), sub, capability.ExecuteInput{
		Name: name, Args: toolArgs, ApprovalToken: approval,
	})
	// Reliable audit of every capability execution (EAP strengthens Den here).
	ev := audit.NewEvent("capability.execute", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.Principal.UserID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Resource = name
	ev.Detail = map[string]any{"capability": name, "kind": res.Kind, "risk": res.RiskLevel, "surface": "mcp/agent"}
	if err != nil {
		ev.Outcome = audit.OutcomeDenied
		if capErr, ok := err.(*capability.Error); ok {
			ev.Detail["error_code"] = capErr.Code
		}
	}
	if auditErr := audit.RequireWrite(r.Context(), s.Store, ev); auditErr != nil {
		writeMCPRPCError(w, r, id, mcp.CodeInternalError, "audit delivery failed — execution refused")
		return
	}
	if err != nil {
		capErr, ok := err.(*capability.Error)
		if !ok {
			capErr = &capability.Error{Code: "internal", Message: err.Error()}
		}
		writeMCPResult(w, r, id, mcp.ToolCallResult{
			IsError: true,
			Content: []mcp.TextContent{{Type: "text", Text: capabilityErrorJSON(capErr)}},
		})
		return
	}
	// DLP egress on the released content (subject-aware, same as direct mount).
	released := s.dlpScanEmployeeText(r, session, res.Content)
	writeMCPResult(w, r, id, mcp.ToolCallResult{
		Content: []mcp.TextContent{{Type: "text", Text: released}},
	})
}

func capabilityErrorJSON(e *capability.Error) string {
	payload := map[string]any{
		"error":   e.Code,
		"message": e.Message,
		"retry":   map[string]any{"search": e.RetrySearch, "action": e.RetryAction},
	}
	if e.ApprovalHint != "" {
		payload["approval_hint"] = e.ApprovalHint
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

var _ = strings.TrimSpace
