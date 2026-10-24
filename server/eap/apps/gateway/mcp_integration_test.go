//go:build integration

// MCP slice integration flow (TASK-004 acceptance): an authorized employee
// completes discovery and a read tool call without ever seeing the upstream
// credential; cross-workspace discovery and execution are denied and recorded.
package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPSliceIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()

	// Point the seeded git tool's registry endpoint at a stub upstream so the
	// execution path is exercised end to end; the agent never sees this URL.
	stubToken := "git-secret-token"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+stubToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"repo":"acme/app","path":"README.md","content":"# hello"}`))
	}))
	t.Cleanup(upstream.Close)
	if _, err := env.pool.Exec(ctx,
		`UPDATE mcp_tools SET endpoint_url = $1 WHERE name = 'git_read_file'`,
		upstream.URL+"/repos/{repo}/contents/{path}"); err != nil {
		t.Fatalf("repoint tool endpoint: %v", err)
	}
	t.Setenv("EAP_MCP_GIT_TOKEN", stubToken)

	// dev-login syncs the user and the secops membership, then we mint a key.
	mcpLogin(t, env, "mcp@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "mcp@example.com"))
	callMCP := func(method, body string) map[string]any {
		t.Helper()
		rec := env.do(t, http.MethodPost, "/mcp/gitlab", keyValue, body,
			map[string]string{"X-Agent-Workspace": "secops"})
		if rec.Code != http.StatusOK {
			t.Fatalf("mcp %s: %d %s", method, rec.Code, rec.Body.String())
		}
		var payload map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
		return payload
	}

	// initialize
	payload := callMCP("initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if fmt.Sprint(payload["result"]) == "" {
		t.Fatalf("initialize returned no result")
	}

	// discovery: the secops workspace sees git_read_file on the gitlab server
	payload = callMCP("tools/list", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	resultJSON, _ := json.Marshal(payload["result"])
	if !strings.Contains(string(resultJSON), "git_read_file") {
		t.Fatalf("authorized discovery must list git_read_file: %s", resultJSON)
	}

	// execution: permitted caller completes a read tool call; the upstream
	// credential must never appear in the response.
	payload = callMCP("tools/call", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"acme/app","path":"README.md"}}}`)
	resultJSON, _ = json.Marshal(payload["result"])
	if !strings.Contains(string(resultJSON), "hello") {
		t.Fatalf("tool call must return the upstream payload: %s", resultJSON)
	}
	if strings.Contains(string(resultJSON), stubToken) {
		t.Fatalf("upstream credential leaked to the agent: %s", resultJSON)
	}

	// cross-workspace: a finance-member caller gets no tools and no results.
	mcpLogin(t, env, "mcp-finance@example.com", "finance")
	financeKey := seedKey(t, env, mcpUserID(t, env, "mcp-finance@example.com"))
	callMCPFinance := func(method, body string) map[string]any {
		t.Helper()
		rec := env.do(t, http.MethodPost, "/mcp/gitlab", financeKey, body,
			map[string]string{"X-Agent-Workspace": "finance"})
		if rec.Code != http.StatusOK {
			t.Fatalf("mcp finance %s: %d %s", method, rec.Code, rec.Body.String())
		}
		var payload map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
		return payload
	}
	payload = callMCPFinance("tools/list", `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if resultJSON, _ := json.Marshal(payload["result"]); strings.Contains(string(resultJSON), "git_read_file") {
		t.Fatalf("cross-workspace discovery leaked a tool: %s", resultJSON)
	}
	payload = callMCPFinance("tools/call", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"acme/app","path":"README.md"}}}`)
	if payload["error"] == nil {
		t.Fatalf("cross-workspace execution must fail")
	}

	// The explicit cross-workspace execution attempt is a recorded denial;
	// discovery isolation is enforced by filtering (the tool never appears).
	var denied int
	if err := env.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE type = 'mcp.tool.executed' AND outcome = 'denied'`).Scan(&denied); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if denied < 1 {
		t.Fatalf("cross-workspace execution denial must be recorded")
	}
	var allowed int
	if err := env.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE type = 'mcp.tool.executed' AND outcome = 'allowed'`).Scan(&allowed); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if allowed < 1 {
		t.Fatalf("execution audit evidence missing")
	}
}

func mcpLogin(t *testing.T, env *sliceEnv, email, workspace string) string {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"workspaces":[%q]}`, email, workspace)
	rec := env.do(t, http.MethodPost, "/enterprise/v1/auth/dev-login", "", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev-login %s: %d %s", email, rec.Code, rec.Body.String())
	}
	var session struct {
		SessionToken string `json:"session_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &session)
	return session.SessionToken
}

func mcpUserID(t *testing.T, env *sliceEnv, email string) string {
	t.Helper()
	var id string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("load user %s: %v", email, err)
	}
	return id
}
