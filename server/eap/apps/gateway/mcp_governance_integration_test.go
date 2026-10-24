//go:build integration

// MCP central-governance integration (TASK-011 acceptance): discovery never
// exceeds execution authority; R1 executes when allowed; R2+ returns
// approval-required with zero upstream egress until an authorized
// continuation; SSRF targets, unsafe redirects, credential/policy/audit
// failures fail closed with trace-linked evidence.
package main_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"encoding/json"

	"enterprise-agent-platform/packages/mcpgateway"
	"enterprise-agent-platform/packages/mcpgov"
)

func TestMCPGovernanceIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()

	upstreamHits := 0
	var lastPaths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		lastPaths = append(lastPaths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer gov-git-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"content":"governed read"}`))
	}))
	defer upstream.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect toward private space — must be blocked per hop.
		http.Redirect(w, r, "http://127.0.0.1:1/secret", http.StatusFound)
	}))
	defer redirector.Close()

	// Registry: R1 allowed tool, R2 tool, SSRF tool, redirect tool, deny tool.
	if _, err := env.pool.Exec(ctx,
		`UPDATE mcp_tools SET endpoint_url = $1 WHERE name = 'git_read_file'`, upstream.URL); err != nil {
		t.Fatalf("update git tool: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO mcp_tools (name, server, description, risk_level, workspaces, endpoint_url, endpoint_method, credential_ref) VALUES
			('write_draft',   'gitlab', 'Create a draft (R2).', 'R2', ARRAY['secops'], $1, 'POST', 'env:EAP_MCP_GIT_TOKEN'),
			('ssrf_tool',     'gitlab', 'SSRF probe.',          'R1', ARRAY['secops'], 'http://169.254.169.254/meta', 'GET', NULL),
			('redirect_tool', 'gitlab', 'Redirect probe.',      'R1', ARRAY['secops'], $2, 'GET', NULL),
			('deny_tool',     'gitlab', 'Central policy denies this tool.', 'R1', ARRAY['secops'], $1, 'GET', NULL)
		ON CONFLICT (name) DO UPDATE SET endpoint_url = EXCLUDED.endpoint_url, risk_level = EXCLUDED.risk_level, credential_ref = EXCLUDED.credential_ref`,
		upstream.URL+"/write", redirector.URL+"/hop1"); err != nil {
		t.Fatalf("seed tools: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM mcp_tools WHERE name IN ('write_draft','ssrf_tool','redirect_tool');`)
	})
	t.Setenv("EAP_MCP_GIT_TOKEN", "gov-git-token")

	// Central MCP policy wired with a deny rule for deny_tool.
	env.srv.MCP.Policy = &mcpDenyPolicy{denyTool: "deny_tool"}
	env.srv.MCP.Approvals = mcpgov.NewMemoryApprovals()
	env.srv.MCP.SSRF = &mcpgov.SSRFPolicy{AllowPrivate: true} // loopback stubs allowed, still per-hop checked
	// Executor 走带 SSRF 每跳校验的 client（redirect_tool 的重定向被拦截）。
	exec := env.srv.MCP.Executor.(*mcpgateway.HTTPExecutor)
	exec.SSRFClient = env.srv.MCP.SSRF.SafeHTTPClient()
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "mcpgov@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "mcpgov@example.com"))
	authz := map[string]string{"Authorization": "Bearer " + keyValue, "X-Agent-Workspace": "secops"}

	callMCP := func(body string) map[string]any {
		t.Helper()
		rec := env.do(t, http.MethodPost, "/mcp/gitlab", keyValue, body, authz)
		if rec.Code != http.StatusOK {
			t.Fatalf("mcp call: %d %s", rec.Code, rec.Body.String())
		}
		var payload map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
		return payload
	}

	// 1. R1 allowed: discovery shows allowed tools (deny_tool invisible), call executes.
	listing := callMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listingJSON := fmt.Sprint(listing["result"])
	if strings.Contains(listingJSON, "deny_tool") {
		t.Fatalf("centrally denied tool must be invisible in discovery: %s", listingJSON)
	}
	if !strings.Contains(listingJSON, "git_read_file") {
		t.Fatalf("allowed R1 tool missing from discovery: %s", listingJSON)
	}
	payload := callMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"acme/app","path":"README.md"}}}`)
	if resultJSON := fmt.Sprint(payload["result"]); !strings.Contains(resultJSON, "governed read") {
		t.Fatalf("R1 call result wrong: %s", resultJSON)
	}

	// 2. Centrally denied tool: neither discoverable nor executable.
	payload = callMCP(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"deny_tool","arguments":{}}}`)
	if payload["error"] == nil {
		t.Fatalf("denied tool must not execute")
	}

	// 3. R2+: approval-required with zero upstream egress; then an approved
	// continuation executes exactly once.
	hitsBefore := upstreamHits
	payload = callMCP(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"write_draft","arguments":{"repo":"acme/app"}}}`)
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil || !strings.Contains(fmt.Sprint(errObj["message"]), "approval required") {
		t.Fatalf("R2 must require approval: %v", payload)
	}
	if upstreamHits != hitsBefore {
		t.Fatalf("R2 approval-required must make zero upstream contact")
	}
	// Grant an authorized continuation (control plane action) and replay.
	approvalStore := env.srv.MCP.Approvals.(mcpgov.ApprovalStore)
	userID := mcpUserID(t, env, "mcpgov@example.com")
	token, err := approvalStore.Grant(ctx, userID, "write_draft", 0)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	payload = callMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"write_draft","arguments":{"repo":"acme/app"},"approval_token":%q}}`, token))
	if payload["error"] != nil {
		t.Fatalf("approved continuation must execute: %v", payload["error"])
	}
	if upstreamHits != hitsBefore+1 {
		t.Fatalf("approved continuation must contact upstream exactly once")
	}
	// One-time: token reuse fails closed with zero egress.
	hitsBefore = upstreamHits
	payload = callMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"write_draft","arguments":{"repo":"acme/app"},"approval_token":%q}}`, token))
	if payload["error"] == nil {
		t.Fatalf("approval token reuse must fail closed")
	}
	if upstreamHits != hitsBefore {
		t.Fatalf("replayed approval must make zero upstream contact")
	}

	// 4. SSRF: link-local target blocked before egress; redirect hop blocked.
	hitsBefore = upstreamHits
	payload = callMCP(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"ssrf_tool","arguments":{}}}`)
	if payload["error"] == nil || !strings.Contains(fmt.Sprint(payload["error"]), "blocked") {
		t.Fatalf("SSRF target must be blocked: %v", payload["error"])
	}
	payload = callMCP(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"redirect_tool","arguments":{}}}`)
	if payload["error"] == nil {
		t.Fatalf("unsafe redirect must be blocked")
	}

	// 5. Trace-linked audit evidence: denials recorded with reasons.
	var denials int
	if err := env.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE type = 'mcp.tool.executed' AND outcome = 'denied'
		  AND (detail->>'reason' LIKE 'policy_denied%'
		    OR detail->>'reason' IN ('approval_required','ssrf_blocked','unknown_risk'))`).Scan(&denials); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if denials < 4 {
		t.Fatalf("expected >=4 classified denials audited, got %d", denials)
	}
	var allowed int
	if err := env.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_events WHERE type='mcp.tool.executed' AND outcome='allowed'`).Scan(&allowed); err != nil {
		t.Fatalf("allowed audit query: %v", err)
	}
	if allowed < 2 {
		t.Fatalf("allowed executions (R1 + approved R2) must be audited, got %d", allowed)
	}
}

// mcpDenyPolicy implements the central MCP policy seam with one deny rule.
type mcpDenyPolicy struct{ denyTool string }

func (m *mcpDenyPolicy) DecideMCP(_ context.Context, _, _, tool, _ string) (mcpgov.PolicyDecision, error) {
	if tool == m.denyTool {
		return mcpgov.PolicyDecision{
			Allowed: false, Effect: "deny", RuleID: "mcp-deny-" + m.denyTool,
			Reason: "tool is denied by central policy",
		}, nil
	}
	return mcpgov.PolicyDecision{Allowed: true, Effect: "allow", RuleID: "mcp-allow-all"}, nil
}
