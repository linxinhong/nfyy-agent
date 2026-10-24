package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/mcpgateway"
	mcp "enterprise-agent-platform/packages/protocol-mcp"
)

// handleMCP implements the minimal Streamable-HTTP MCP surface
// (POST /mcp/{server}, JSON responses) over the governed mcpgateway service.
//
// Authentication and workspace resolution are identical to the enterprise
// API: the employee API key authenticates, the X-Agent-Workspace header
// selects, and membership decides. Discovery and execution both run through
// the same workspace policy inside the service.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.MCP == nil {
		writeProblem(w, r, errs.New(errs.NotFound, "mcp gateway is not configured"))
		return
	}
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		ws, origin := workspaceSelector(r, auth)
		s.auditAuthFailedDetail(r, &auth.Principal.UserID, "api_key", map[string]any{
			"reason":           "workspace_denied",
			"workspace":        ws,
			"workspace_origin": origin,
			"server":           r.PathValue("server"),
		})
		writeProblem(w, r, err)
		return
	}
	// Employee-facing DLP context for egress scans (tools/call result).
	r = withDLPSubject(r, session.Workspace.WorkspaceID, session.Workspace.Roles, false)

	server := r.PathValue("server")
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
	if envelope.JSONRPC != "2.0" || envelope.Method == "" {
		writeMCPRPCError(w, r, envelope.ID, mcp.CodeInvalidRequest, "invalid JSON-RPC 2.0 envelope")
		return
	}

	switch envelope.Method {
	case "initialize":
		if sid := mcpSessionID(); sid != "" {
			w.Header().Set("Mcp-Session-Id", sid)
		}
		writeMCPResult(w, r, envelope.ID, mcp.InitializeResult{
			ProtocolVersion: mcp.ProtocolVersion,
			Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{}},
			ServerInfo:      mcp.Implementation{Name: "eap-mcp-gateway", Version: Version},
		})
	case "tools/list":
		tools, err := s.MCP.ListTools(r.Context(), session, server)
		if err != nil {
			writeMCPRPCError(w, r, envelope.ID, mcp.CodeInternalError, rpcMessage(err))
			return
		}
		mcpTools := make([]mcp.Tool, 0, len(tools))
		for _, tool := range tools {
			mcpTools = append(mcpTools, mcp.Tool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: mcp.JSONSchema{Type: "object"},
			})
		}
		writeMCPResult(w, r, envelope.ID, mcp.ToolsListResult{Tools: mcpTools})
	case "tools/call":
		var params mcp.ToolCallParams
		if len(envelope.Params) > 0 {
			if err := json.Unmarshal(envelope.Params, &params); err != nil {
				writeMCPRPCError(w, r, envelope.ID, mcp.CodeInvalidParams, "tools/call params must be an object")
				return
			}
		}
		result, err := s.MCP.CallTool(r.Context(), session, mcpgateway.ToolCall{
			Server:        server,
			Name:          params.Name,
			Arguments:     params.Arguments,
			ApprovalToken: params.ApprovalToken,
		})
		if err != nil {
			problem, ok := errs.From(err)
			if !ok {
				problem = errs.Problem{Code: errs.Internal, Message: "tool call failed"}
			}
			writeMCPRPCError(w, r, envelope.ID, mcpRPCCode(problem.Code), problem.Message)
			return
		}
		// Egress DLP scan of the tool response (TASK-029): employee-facing
		// surface with subject exemptions; tokenize/redact rewrite the text,
		// deny-class content is withheld with the citation-style marker.
		released := s.dlpScanEmployeeText(r, session, string(result.Result))
		writeMCPResult(w, r, envelope.ID, mcp.ToolCallResult{
			Content: []mcp.TextContent{{Type: "text", Text: released}},
		})
	default:
		writeMCPRPCError(w, r, envelope.ID, mcp.CodeMethodNotFound, "method not supported by this server")
	}
}

// mcpRPCCode maps frozen EAP codes onto JSON-RPC error codes. A tool that is
// not visible to the workspace is indistinguishable from an unknown tool.
func mcpRPCCode(code errs.Code) int {
	switch code {
	case errs.InvalidArgument:
		return mcp.CodeInvalidParams
	case errs.NotFound:
		return mcp.CodeMethodNotFound
	default:
		return mcp.CodeInternalError
	}
}

func rpcMessage(err error) string {
	if problem, ok := errs.From(err); ok {
		return problem.Message
	}
	return err.Error()
}

// wantsSSE reports whether the client negotiates the Streamable-HTTP SSE
// response form (MCP spec: honor the Accept header's text/event-stream).
func wantsSSE(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		if strings.HasPrefix(strings.TrimSpace(part), "text/event-stream") {
			return true
		}
	}
	return false
}

// writeMCPMessage emits the JSON-RPC message as SSE when the client asked
// for it, plain JSON otherwise. Both are spec-conforming responses; the
// stream ends after the single message (stateless surface).
func writeMCPMessage(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	b, _ := json.Marshal(payload)
	if wantsSSE(r) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func writeMCPResult(w http.ResponseWriter, r *http.Request, id json.RawMessage, result any) {
	writeMCPMessage(w, r, map[string]any{
		"jsonrpc": "2.0",
		"id":      rawJSONID(id),
		"result":  result,
	})
}

func writeMCPRPCError(w http.ResponseWriter, r *http.Request, id json.RawMessage, code int, message string) {
	writeMCPMessage(w, r, map[string]any{
		"jsonrpc": "2.0",
		"id":      rawJSONID(id),
		"error":   map[string]any{"code": code, "message": message},
	})
}

// mcpSessionID issues a stateless session handle (MCP spec: the server MAY
// assign one at initialize; stateless servers may accept any or none).
func mcpSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// rawJSONID preserves the request's id verbatim (number or string).
func rawJSONID(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}
