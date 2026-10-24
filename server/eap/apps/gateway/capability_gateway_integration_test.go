//go:build integration

// Capability Gateway integration (TASK-034, ADR-0023): permission-aware
// discovery (workspace scoping, R2+ needs_approval, policy denial status),
// execute revalidation (approval/DLP/audit), dual-surface governance parity
// with the direct /mcp/{server} mount, and the skill guidance path.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/capability"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/mcpgateway"
	"enterprise-agent-platform/packages/mcpgov"
	"enterprise-agent-platform/packages/policy"
)

func buildCapabilityGateway(t *testing.T, env *sliceEnv, mcpPolicy mcpgov.PolicyEngine) {
	t.Helper()
	mcpListAll := func(ctx context.Context, workspaceID string) ([]mcpgateway.RegisteredTool, error) {
		rows, err := env.pool.Query(ctx, `SELECT DISTINCT server FROM mcp_tools ORDER BY server`)
		if err != nil {
			return nil, err
		}
		var servers []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return nil, err
			}
			servers = append(servers, s)
		}
		rows.Close()
		var out []mcpgateway.RegisteredTool
		for _, srv := range servers {
			tools, err := env.store.ListToolsForWorkspace(ctx, srv, workspaceID)
			if err != nil {
				return nil, err
			}
			out = append(out, tools...)
		}
		return out, nil
	}
	env.srv.Capabilities = &capability.Registry{
		Sources: []capability.Source{
			&capability.MCPSource{Service: env.srv.MCP, Policy: mcpPolicy, ListTools: mcpListAll},
			&capability.SkillSource{
				ListSkills: func(ctx context.Context, workspaceID string) ([]capability.SkillEntry, error) {
					rows, err := env.store.ListReleasedSkillsForWorkspace(ctx, workspaceID)
					if err != nil {
						return nil, err
					}
					out := make([]capability.SkillEntry, 0, len(rows))
					for _, r := range rows {
						out = append(out, capability.SkillEntry{Name: r.Name, Version: r.Version, Description: "released skill package"})
					}
					return out, nil
				},
				ResolveSkill: func(ctx context.Context, workspaceID, name, version string) (capability.SkillEntry, error) {
					e, err := env.store.ResolveReleasedSkill(ctx, workspaceID, name, version)
					if err != nil {
						return capability.SkillEntry{}, err
					}
					return capability.SkillEntry{Name: e.Name, Version: e.Version, Description: "released skill package"}, nil
				},
			},
		},
	}
}

func agentCall(t *testing.T, env *sliceEnv, key string, method string, params string) (int, string) {
	t.Helper()
	rec := env.do(t, http.MethodPost, "/mcp/agent", key,
		`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`,
		map[string]string{"X-Agent-Workspace": "secops"})
	return rec.Code, rec.Body.String()
}

func TestCapabilityGateway(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.MCP.Policy = &mcpDenyPolicy{denyTool: "git_forbidden"}
	env.srv.MCP.Approvals = mcpgov.NewMemoryApprovals()
	env.srv.MCP.SSRF = &mcpgov.SSRFPolicy{AllowPrivate: true}
	if exec, ok := env.srv.MCP.Executor.(*mcpgateway.HTTPExecutor); ok {
		exec.SSRFClient = env.srv.MCP.SSRF.SafeHTTPClient()
	}
	env.srv.Policy = &policy.TablePolicy{LoadRules: env.store.LoadPolicyRules}
	buildCapabilityGateway(t, env, env.srv.MCP.Policy)
	env.handler = env.srv.Handler()

	// Live upstream stub the R1 execution actually calls.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":"file-bytes","path":"README.md"}`))
	}))
	t.Cleanup(upstream.Close)

	// Seed: an R1 tool, an R3 tool (approval-gated), a policy-denied tool,
	// a finance-only tool, and a released skill.
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO mcp_tools (name, server, description, risk_level, workspaces, endpoint_url, endpoint_method, credential_ref) VALUES
		 ('git_read_file','gitlab','Read a file from an authorized repository.','R1','{secops}',$1,'GET','env:EAP_MCP_GIT_TOKEN'),
		 ('git_push','gitlab','Push commits to the repository.','R3','{secops}',$1 || '/push','POST','env:EAP_MCP_GIT_TOKEN'),
		 ('git_forbidden','gitlab','Forbidden by policy.','R1','{secops}',$1 || '/forbidden','GET',''),
		 ('fin_report','finance','Finance-only report.','R1','{finance}',$1 || '/fin','GET','')
		ON CONFLICT (name) DO UPDATE SET endpoint_url = EXCLUDED.endpoint_url, endpoint_method = EXCLUDED.endpoint_method,
		     risk_level = EXCLUDED.risk_level, workspaces = EXCLUDED.workspaces, credential_ref = EXCLUDED.credential_ref`,
		upstream.URL+"/repos/{repo}/contents/{path}"); err != nil {
		t.Fatalf("seed tools: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO skill_packages (name, version, workspace_id, status, digest, payload, published_by)
		VALUES ('cap-skill','1.0.0','secops','released','d1','bytes','usr_seed')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM mcp_tools WHERE server='gitlab' OR name='fin_report';
			DELETE FROM skill_packages WHERE name='cap-skill'; DELETE FROM audit_events WHERE resource LIKE 'mcp:%';`)
	})

	mcpLogin(t, env, "cap-user@example.com", "secops")
	key := seedKey(t, env, mcpUserID(t, env, "cap-user@example.com"))

	// 1. tools/list is exactly two constant meta-tools.
	code, body := agentCall(t, env, key, "tools/list", `{}`)
	if code != http.StatusOK || !strings.Contains(body, "search_capabilities") || !strings.Contains(body, "execute_capability") {
		t.Fatalf("tools/list: %d %s", code, body)
	}
	// The invariant is a constant TWO-tool list, not the absence of example
	// strings in schema descriptions: parse and assert the tool names.
	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("tools/list parse: %v", err)
	}
	if len(listed.Result.Tools) != 2 {
		t.Fatalf("meta surface must stay exactly two tools, got %d: %s", len(listed.Result.Tools), body)
	}

	// 2. search is permission-aware: secops tools visible, finance tool absent,
	//    policy-denied flagged, R3 flagged needs_approval, skill listed.
	code, body = agentCall(t, env, key, "tools/call",
		`{"name":"search_capabilities","arguments":{"query":"git file push finance report skill","limit":20}}`)
	if code != http.StatusOK {
		t.Fatalf("search: %d %s", code, body)
	}
	if !strings.Contains(body, "mcp:gitlab:git_read_file") {
		t.Fatalf("R1 tool missing from search: %s", body)
	}
	if strings.Contains(body, `"name":"fin_report"`) || strings.Contains(body, "mcp:finance:fin_report") {
		t.Fatalf("finance capability leaked into secops search: %s", body)
	}
	if !strings.Contains(body, "needs_approval") {
		t.Fatalf("R3 push must surface needs_approval: %s", body)
	}
	if !strings.Contains(body, "denied_by_policy") {
		t.Fatalf("policy-denied tool must surface denied_by_policy: %s", body)
	}
	if !strings.Contains(body, "skill:cap-skill") {
		t.Fatalf("released skill missing from search: %s", body)
	}

	// 3. execute on R1 runs the same chain as the direct mount (audit + result).
	t.Setenv("EAP_MCP_GIT_TOKEN", "cap-git-token")
	code, body = agentCall(t, env, key, "tools/call",
		`{"name":"execute_capability","arguments":{"name":"mcp:gitlab:git_read_file","arguments":{"repo":"acme/api","path":"README.md"}}}`)
	if code != http.StatusOK || strings.Contains(body, "is_error") {
		t.Fatalf("execute R1: %d %s", code, body)
	}

	// 4. execute on R3 without approval fails closed (structured error).
	code, body = agentCall(t, env, key, "tools/call",
		`{"name":"execute_capability","arguments":{"name":"mcp:gitlab:git_push","arguments":{"repo":"acme/api"}}}`)
	if code != http.StatusOK || !strings.Contains(body, "needs_approval") {
		t.Fatalf("R3 without approval must fail with needs_approval: %d %s", code, body)
	}

	// 5. finance member: finance capability visible, secops absent.
	mcpLogin(t, env, "cap-fin@example.com", "finance")
	finKey := seedKey(t, env, mcpUserID(t, env, "cap-fin@example.com"))
	finRec := env.do(t, http.MethodPost, "/mcp/agent", finKey,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_capabilities","arguments":{"query":"git finance report","limit":20}}}`,
		map[string]string{"X-Agent-Workspace": "finance"})
	code, body = finRec.Code, finRec.Body.String()
	if code != http.StatusOK || strings.Contains(body, "gitlab") || !strings.Contains(body, "fin_report") {
		t.Fatalf("finance view wrong: %d %s", code, body)
	}

	// 6. skill execute returns governed guidance, never runs package code.
	code, body = agentCall(t, env, key, "tools/call",
		`{"name":"execute_capability","arguments":{"name":"skill:cap-skill"}}`)
	if code != http.StatusOK || !strings.Contains(body, "eap skill install cap-skill@1.0.0") {
		t.Fatalf("skill guidance: %d %s", code, body)
	}

	// 7. unknown capability returns the structured taxonomy.
	code, body = agentCall(t, env, key, "tools/call",
		`{"name":"execute_capability","arguments":{"name":"mcp:nowhere:nothing"}}`)
	if code != http.StatusOK || !strings.Contains(body, "unknown_capability") {
		t.Fatalf("unknown capability: %d %s", code, body)
	}

	// 8. every execution audited (allowed + denied).
	var allowedEv, deniedEv int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type='capability.execute' AND outcome='allowed' AND resource='mcp:gitlab:git_read_file'`).Scan(&allowedEv); err != nil || allowedEv < 1 {
		t.Fatalf("allowed capability audit missing: %d %v", allowedEv, err)
	}
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type='capability.execute' AND outcome='denied' AND resource='mcp:gitlab:git_push'`).Scan(&deniedEv); err != nil || deniedEv < 1 {
		t.Fatalf("denied capability audit missing: %d %v", deniedEv, err)
	}
	_ = audit.OutcomeAllowed
}
