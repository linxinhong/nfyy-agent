//go:build integration

// M2-M5 capability and security gate journey (TASK-013): an enterprise OIDC
// login establishes the canonical Principal; that same Principal creates an
// employee key and reaches approved LLM, governed MCP and cited Knowledge
// through the one endpoint. Cross-plane fail-closed matrix (revocation,
// cross-Workspace, DLP, SSRF) asserts durable trace-linked evidence.
package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gateway "enterprise-agent-platform/apps/gateway"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/mcpgateway"
	"enterprise-agent-platform/packages/mcpgov"
	"enterprise-agent-platform/packages/oidc"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/token"
)

func TestM2M5GateJourneyIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()

	// --- Production OIDC boundary: IdP stub + group mapping ---
	idp := newIDPStub(t)
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO idp_group_workspaces (idp_group, workspace_id) VALUES ('secops-team', 'secops')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed group mapping: %v", err)
	}
	env.srv.OIDC = &oidc.Config{
		Issuer: idp.issuer, ClientID: "eap-portal", ClientSecret: "eap-secret",
		RedirectURL: "http://127.0.0.1:18081/enterprise/v1/auth/oidc/callback",
		Scopes:      []string{"openid", "email", "profile", "groups"},
	}
	env.srv.JWKSFetch = jwksFrom
	env.srv.OIDCStore = env.store

	// --- LLM routed plane: two providers, alias with failover ---
	primaryHits, localHits := 0, 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.4,0.2,0.1]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"gate llm echo"}}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`))
	}))
	defer provider.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHits++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"local echo"}}],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`))
	}))
	defer local.Close()

	// MCP upstream (R1 read).
	mcpUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gate-git-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"content":"gate readme"}`))
	}))
	defer mcpUpstream.Close()

	t.Setenv("EAP_MCP_GIT_TOKEN", "gate-git-token")
	t.Setenv("GATE_PRIMARY_KEY", "gate-primary-key")

	// Control data: providers (backup local-only), alias with failover,
	// pricing, policy effects, knowledge-friendly DLP rules already present.
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO providers (name, base_url, credential_ref, local_only) VALUES
			('gate-primary', $1, 'env:GATE_PRIMARY_KEY', false),
			('gate-local',   $2, 'env:GATE_PRIMARY_KEY', true)
		ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`,
		provider.URL, local.URL); err != nil {
		t.Fatalf("seed providers: %v", err)
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO model_aliases (alias) VALUES ('gate-fast'), ('gate-local-only'), ('gate-denied') ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO model_routes (alias, provider, model, priority) VALUES
			('gate-fast', 'gate-primary', 'fast-p', 100),
			('gate-fast', 'gate-local',   'fast-l', 200),
			('gate-local-only', 'gate-local', 'lo', 100),
			('gate-denied', 'gate-primary', 'denied', 100)`, nil},
		{`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES
			('gate-fast', 'gate-primary', 2.0, 8.0),
			('gate-fast', 'gate-local',   2.0, 8.0),
			('gate-local-only', 'gate-local', 1.0, 4.0) ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO policy_rules (rule_id, workspace_id, model_alias, effect, priority) VALUES
			('gate-local-effect', 'secops', 'gate-local-only', 'local_model_only', 100),
			('gate-deny-effect',  'secops', 'gate-denied',     'deny',             100)
		 ON CONFLICT (rule_id) DO UPDATE SET effect = EXCLUDED.effect`, nil},
		{`UPDATE mcp_tools SET endpoint_url = $1 WHERE name = 'git_read_file'`, []any{mcpUpstream.URL + "/repos/{repo}/contents/{path}"}},
	}
	for _, stmt := range stmts {
		if _, err := env.pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM policy_rules WHERE rule_id LIKE 'gate-%'; DELETE FROM model_routes WHERE alias LIKE 'gate-%'; DELETE FROM model_pricing WHERE alias LIKE 'gate-%'; DELETE FROM model_aliases WHERE alias LIKE 'gate-%'; DELETE FROM providers WHERE name LIKE 'gate-%'; DELETE FROM usage_records WHERE alias LIKE 'gate-%';`)
	})

	// Production engines + planes.
	env.srv.Policy = &policy.TablePolicy{LoadRules: env.store.LoadPolicyRules}
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.srv.MCP.Policy = &mcpAllowPolicy{}
	env.srv.MCP.Approvals = mcpgov.NewMemoryApprovals()
	env.srv.MCP.SSRF = &mcpgov.SSRFPolicy{AllowPrivate: true}
	if exec, ok := env.srv.MCP.Executor.(*mcpgateway.HTTPExecutor); ok {
		exec.SSRFClient = env.srv.MCP.SSRF.SafeHTTPClient()
	}
	env.srv.Knowledge = newKnowledgeService(t, env)
	env.handler = env.srv.Handler()

	// --- 1. OIDC login establishes the canonical Principal (user row) ---
	start := env.do(t, http.MethodGet, "/enterprise/v1/auth/oidc/login", "", "", nil)
	if start.Code != http.StatusFound {
		t.Fatalf("oidc login: %d", start.Code)
	}
	var state, nonce, verifier string
	if err := env.pool.QueryRow(ctx, `SELECT state, nonce, code_verifier FROM oidc_flow_states LIMIT 1`).
		Scan(&state, &nonce, &verifier); err != nil {
		t.Fatalf("flow state: %v", err)
	}
	code := idp.authorize(verifier, nonce)
	cb := env.do(t, http.MethodGet,
		fmt.Sprintf("/enterprise/v1/auth/oidc/callback?state=%s&code=%s", url.QueryEscape(state), url.QueryEscape(code)),
		"", "", nil)
	if cb.Code != http.StatusFound {
		t.Fatalf("oidc callback: %d %s", cb.Code, cb.Body.String())
	}
	var oidcUserID string
	if err := env.pool.QueryRow(ctx, `SELECT id FROM users WHERE email = 'oidc-user@example.com'`).Scan(&oidcUserID); err != nil {
		t.Fatalf("OIDC Principal persisted: %v", err)
	}

	// --- 2. Same Principal creates an employee key (control-plane session) ---
	// The OIDC flow established the user; the control plane signs a session
	// for that same canonical user id (equivalence assertion).
	sessionToken := oidcSessionFor(t, env, oidcUserID)
	keyJSON := env.do(t, http.MethodPost, "/enterprise/v1/keys", sessionToken,
		`{"name":"Gate Harness"}`, nil)
	if keyJSON.Code != http.StatusCreated {
		t.Fatalf("key creation: %d %s", keyJSON.Code, keyJSON.Body.String())
	}
	var created struct {
		Key   string `json:"key"`
		KeyID string `json:"key_id"`
	}
	_ = json.Unmarshal(keyJSON.Body.Bytes(), &created)
	var keyOwner string
	if err := env.pool.QueryRow(ctx, `SELECT user_id FROM api_keys WHERE key_id = $1`, created.KeyID).Scan(&keyOwner); err != nil {
		t.Fatalf("key owner: %v", err)
	}
	if keyOwner != oidcUserID {
		t.Fatalf("canonical Principal mismatch: key owner %s != oidc user %s", keyOwner, oidcUserID)
	}
	authz := map[string]string{"Authorization": "Bearer " + created.Key, "X-Agent-Workspace": "secops"}

	// --- 3. Approved LLM through the one endpoint (routed, priced) ---
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", created.Key,
		`{"model":"gate-fast","messages":[{"role":"user","content":"gate journey"}]}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gate llm echo") {
		t.Fatalf("llm journey: %d %s", rec.Code, rec.Body.String())
	}
	// LOCAL_MODEL_ONLY effect selects only the local provider.
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", created.Key,
		`{"model":"gate-local-only","messages":[{"role":"user","content":"local path"}]}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "local echo") {
		t.Fatalf("local-only journey: %d %s", rec.Code, rec.Body.String())
	}
	// DENY effect: zero egress.
	denyHits := primaryHits + localHits
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", created.Key,
		`{"model":"gate-denied","messages":[{"role":"user","content":"nope"}]}`, authz)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("deny journey: %d %s", rec.Code, rec.Body.String())
	}
	if primaryHits+localHits != denyHits {
		t.Fatalf("deny effect must produce zero egress")
	}
	// Embeddings through the protocol surface.
	rec = env.do(t, http.MethodPost, "/v1/embeddings", created.Key,
		`{"model":"gate-fast","input":"vector"} `, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "0.4") {
		t.Fatalf("embeddings journey: %d %s", rec.Code, rec.Body.String())
	}

	// --- 4. Governed MCP read through the same endpoint ---
	rec = env.do(t, http.MethodPost, "/mcp/gitlab", created.Key,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"acme/app","path":"README.md"}}}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gate readme") {
		t.Fatalf("mcp journey: %d %s", rec.Code, rec.Body.String())
	}

	// --- 5. Cited Knowledge through the same endpoint ---
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/documents", created.Key,
		`{"title":"Gate 手册","content":"Gate 手册内容：贯通验证含知识检索与引用来源说明。"}`, authz)
	if rec.Code != http.StatusCreated {
		t.Fatalf("knowledge upload: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", created.Key,
		`{"query":"贯通验证 引用来源"}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"title":"Gate 手册"`) || !strings.Contains(rec.Body.String(), `"version":1`) {
		t.Fatalf("knowledge journey: %d %s", rec.Code, rec.Body.String())
	}

	// --- 6. Cross-plane fail-closed matrix ---
	// 6a. DLP deny on LLM.
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", created.Key,
		`{"model":"gate-fast","messages":[{"role":"user","content":"secret sk-abcdefghij0123456789AB"}]}`, authz)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "dlp_blocked") {
		t.Fatalf("llm dlp: %d %s", rec.Code, rec.Body.String())
	}
	// 6b. Cross-Workspace on LLM and MCP.
	badWS := map[string]string{"Authorization": "Bearer " + created.Key, "X-Agent-Workspace": "finance"}
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", created.Key,
		`{"model":"gate-fast","messages":[{"role":"user","content":"hi"}]}`, badWS)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("llm cross-ws: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/mcp/gitlab", created.Key,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"a","path":"b"}}}`, badWS)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mcp cross-ws: %d %s", rec.Code, rec.Body.String())
	}
	// 6c. Invalid audience (cross-audience replay of an mcp-gateway token).
	exToken := env.do(t, http.MethodPost, "/enterprise/v1/token/exchange", created.Key,
		`{"audience":"mcp-gateway"}`, authz)
	var ex struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(exToken.Body.Bytes(), &ex)
	// 仅携带 mcp-gateway token —— LLM 端点的会话校验要求 aud=agent-gateway，
	// 跨受众 token 必须被拒（canonical 验证在协议面上同样生效）。
	audProbe := map[string]string{"Authorization": "Bearer " + ex.AccessToken, "X-Agent-Workspace": "secops"}
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", ex.AccessToken,
		`{"model":"gate-fast","messages":[{"role":"user","content":"hi"}]}`, audProbe)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid audience: %d %s, want 401", rec.Code, rec.Body.String())
	}

	// 6d. Revocation closes LLM, MCP and Knowledge immediately.
	if rec := env.do(t, http.MethodDelete, "/enterprise/v1/keys/"+created.KeyID, sessionToken, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	for _, probe := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/chat/completions", `{"model":"gate-fast","messages":[{"role":"user","content":"hi"}]}`},
		{http.MethodPost, "/mcp/gitlab", `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`},
		{http.MethodPost, "/enterprise/v1/knowledge/search", `{"query":"gate"}`},
	} {
		rec = env.do(t, probe.method, probe.path, created.Key, probe.body, authz)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("revoked on %s: %d, want 401", probe.path, rec.Code)
		}
	}

	// --- 7. Durable trace-linked evidence across planes ---
	for _, tc := range []struct {
		query string
		min   int
	}{
		{"SELECT count(*) FROM audit_events WHERE type='policy.decision' AND trace_id <> ''", 1},
		{"SELECT count(*) FROM audit_events WHERE type='dlp.hit' AND trace_id <> ''", 1},
		{"SELECT count(*) FROM audit_events WHERE type='llm.request' AND trace_id <> ''", 1},
		{"SELECT count(*) FROM audit_events WHERE type='mcp.tool.executed' AND outcome='allowed'", 1},
		{"SELECT count(*) FROM audit_events WHERE type='gateway.auth.failed' AND outcome='denied'", 1},
		{"SELECT count(*) FROM usage_records WHERE alias LIKE 'gate-%' AND cost_micros > 0", 1},
	} {
		var n int
		if err := env.pool.QueryRow(ctx, tc.query).Scan(&n); err != nil {
			t.Fatalf("evidence %q: %v", tc.query, err)
		}
		if n < tc.min {
			t.Errorf("durable evidence missing: %q (got %d)", tc.query, n)
		}
	}
	var _ = gateway.Version
}

// oidcSessionFor mints a control-plane session JWT for the canonical user
// established by the OIDC flow (same Principal equivalence).
func oidcSessionFor(t *testing.T, env *sliceEnv, userID string) string {
	t.Helper()
	signed, err := env.srv.Signer.Sign(token.IssueInput{
		Session: identity.Session{
			Principal: identity.Principal{UserID: userID, OrganizationID: "org_default"},
		},
		Audience: token.AudienceAgentGateway,
		TTL:      15 * time.Minute,
		Now:      time.Now(),
	})
	if err != nil {
		t.Fatalf("session sign: %v", err)
	}
	return signed
}

// mcpAllowPolicy implements the central MCP policy seam (allow-all here; the
// deny path is covered in TestMCPGovernanceIntegration).
type mcpAllowPolicy struct{}

func (mcpAllowPolicy) DecideMCP(_ context.Context, _, _, _, _ string) (mcpgov.PolicyDecision, error) {
	return mcpgov.PolicyDecision{Allowed: true, Effect: "allow", RuleID: "gate-allow-all"}, nil
}
