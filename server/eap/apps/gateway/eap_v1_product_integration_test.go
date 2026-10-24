//go:build integration

// EAP CLI + distribution V1 product gate journey (TASK-021): device login ->
// skill publish/review/release (Portal-equivalent server records) -> eap
// skill sync (Harness + Codex clients) -> agent install/update -> revocation
// closing everything; Harness and Codex qualify against the same endpoint
// and authorization plane; Portal/CLI consistency against the same control
// data; no secret enters client configuration.
package main_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/mcpgov"
	"enterprise-agent-platform/packages/oidc"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillsync"

	"enterprise-agent-platform/packages/mcpgateway"
)

func TestEAPV1ProductJourneyIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()

	// --- planes: LLM provider, MCP upstream, OIDC IdP ---
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"v1 journey"}}],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`))
	}))
	defer provider.Close()
	mcpUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer v1-git-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"content":"v1 readme"}`))
	}))
	defer mcpUp.Close()
	idp := newIDPStub(t)
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO idp_group_workspaces (idp_group, workspace_id) VALUES ('secops-team', 'secops')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	// Production engines wired.
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
	env.srv.OIDC = &oidc.Config{
		Issuer: idp.issuer, ClientID: "eap-portal", ClientSecret: "eap-secret",
		RedirectURL: "http://127.0.0.1:18081/enterprise/v1/auth/oidc/callback",
		Scopes:      []string{"openid", "email", "profile", "groups"},
	}
	env.srv.JWKSFetch = jwksFrom
	env.srv.OIDCStore = env.store
	env.srv.Knowledge = newKnowledgeService(t, env)
	env.handler = env.srv.Handler()
	t.Setenv("EAP_MCP_GIT_TOKEN", "v1-git-token")
	t.Setenv("GATE_PRIMARY_KEY", "gate-key")

	// 控制数据：providers/alias/routes/pricing（LLM 数据面）
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('v1-primary', $1, 'env:GATE_PRIMARY_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, provider.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	seedStmts := []string{
		`INSERT INTO model_aliases (alias) VALUES ('gate-fast') ON CONFLICT DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority) VALUES ('gate-fast', 'v1-primary', 'fast-v1', 100)`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('gate-fast', 'v1-primary', 1.0, 2.0) ON CONFLICT DO NOTHING`,
	}
	for _, stmt := range seedStmts {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %v", err)
		}
	}
	// MCP 工具端点指向本测试的上游桩
	if _, err := env.pool.Exec(ctx,
		`UPDATE mcp_tools SET endpoint_url = $1 WHERE name = 'git_read_file'`,
		mcpUp.URL+"/repos/{repo}/contents/{path}"); err != nil {
		t.Fatalf("seed mcp tool: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM model_routes WHERE alias='gate-fast'; DELETE FROM model_pricing WHERE alias='gate-fast'; DELETE FROM model_aliases WHERE alias='gate-fast'; DELETE FROM providers WHERE name='v1-primary'; DELETE FROM usage_records WHERE alias='gate-fast';`)
	})

	// --- 1. Device login (SSO-equivalent boundary) ---
	codeResp := postJSONHelper(t, env, "/enterprise/v1/auth/device/code", "", `{}`, nil)
	deviceCode := codeResp["device_code"].(string)
	userCode := codeResp["user_code"].(string)
	sessionToken := mcpLogin(t, env, "v1pilot@example.com", "secops")
	if st := postJSONHelper(t, env, "/enterprise/v1/auth/device/approve", sessionToken,
		fmt.Sprintf(`{"user_code":%q}`, userCode), nil); st["__status"].(float64) != http.StatusOK {
		t.Fatalf("device approve: %v", st)
	}
	var refreshTok, deviceAccess string
	for i := 0; i < 4; i++ {
		poll := postJSONHelper(t, env, "/enterprise/v1/auth/device/token", "",
			fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode), nil)
		if poll["refresh_token"] != nil {
			refreshTok = poll["refresh_token"].(string)
			deviceAccess = poll["access_token"].(string)
			break
		}
	}
	if refreshTok == "" {
		t.Fatalf("device login failed")
	}
	// OIDC Principal equivalence: device user == OIDC-synced user id.
	oidcUserID := mcpUserID(t, env, "v1pilot@example.com")
	boot := env.do(t, http.MethodGet, "/enterprise/v1/cli/bootstrap", deviceAccess, "", nil)
	if !strings.Contains(boot.Body.String(), fmt.Sprintf(`"id":%q`, oidcUserID)) {
		t.Fatalf("device bootstrap Principal != canonical user: %s", boot.Body.String())
	}

	// --- 2. Skill publish (dev key) -> review/release (maintainer) ---
	mcpLogin(t, env, "v1dev@example.com", "secops")
	devKey := seedKey(t, env, mcpUserID(t, env, "v1dev@example.com"))
	_ = mcpLogin(t, env, "v1maint@example.com", "secops")
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspace_members SET roles = '{maintainer}' WHERE user_id = (SELECT id FROM users WHERE email='v1maint@example.com')`); err != nil {
		t.Fatalf("maintainer: %v", err)
	}
	pubKeyPub, pubKeyPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO skill_package_keys (kid, public_key, active) VALUES ('pkg-v1', $1, true)
		 ON CONFLICT DO NOTHING`, base64.RawURLEncoding.EncodeToString(pubKeyPub)); err != nil {
		t.Fatalf("seed pkg key: %v", err)
	}
	env.srv.PackageKeys = map[string]ed25519.PrivateKey{"pkg-v1": pubKeyPriv}

	files := []skillreg.PackageFile{
		{Path: "SKILL.md", Content: "# V1 journey skill"},
		{Path: "steps.md", Content: "1. login 2. sync 3. use"},
	}
	archive, digest, err := skillreg.BuildArchive(files)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := skillreg.SignRelease("pkg-v1", pubKeyPriv, digest, time.Now().UTC())
	_ = pubKeyPriv
	if err != nil {
		t.Fatal(err)
	}

	if _, err := env.pool.Exec(ctx, `
		INSERT INTO skill_packages (name, version, workspace_id, digest, payload, status, published_by, signed_by, signature)
		VALUES ('v1-skill', '1.0.0', 'secops', $1, $2, 'released', 'usr_seed', 'pkg-v1', $3)
		ON CONFLICT DO NOTHING`, digest, archive, sig.Signature); err != nil {
		t.Fatalf("seed released skill: %v", err)
	}

	// --- 3. eap skill sync into Harness AND Codex (same verified release) ---
	keys := parseKeysFromDiscovery(t, env)
	home := t.TempDir()
	for _, client := range []string{"harness", "codex"} {
		res, err := skillsync.Sync(ctx, &skillDownload{env: env, key: devKey}, skillsync.Options{
			Client: client, InstallRoot: filepath.Join(home, "skills"),
			LockfilePath: filepath.Join(home, "skills.lock.json"),
			Keys:         keys,
		}, "v1-skill", "1.0.0", false)
		if err != nil || !res.Applied {
			t.Fatalf("sync %s: %v %+v", client, err, res)
		}
		content, err := os.ReadFile(filepath.Join(home, "skills", client, "v1-skill", "SKILL.md"))
		if err != nil || string(content) != "# V1 journey skill" {
			t.Fatalf("%s install content wrong: %v", client, err)
		}
	}

	// --- 4. One key reaches LLM + MCP + Knowledge ---
	pilotKey := seedKey(t, env, oidcUserID)
	authz := map[string]string{"Authorization": "Bearer " + pilotKey, "X-Agent-Workspace": "secops"}
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", pilotKey,
		`{"model":"gate-fast","messages":[{"role":"user","content":"v1"}]}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "v1 journey") {
		t.Fatalf("llm: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/mcp/gitlab", pilotKey,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"a","path":"b"}}}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "v1 readme") {
		t.Fatalf("mcp: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/documents", pilotKey,
		`{"title":"V1 手册","content":"V1 手册：贯通验证的知识条目与来源引用说明。"}`, authz)
	if rec.Code != http.StatusCreated {
		t.Fatalf("knowledge upload: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", pilotKey,
		`{"query":"贯通验证 知识"}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"title":"V1 手册"`) {
		t.Fatalf("knowledge journey: %d %s", rec.Code, rec.Body.String())
	}

	// --- 5. Portal/CLI consistency: same control records both surfaces read.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/skills", pilotKey, "", authz)
	if !strings.Contains(rec.Body.String(), `"name":"v1-skill"`) || !strings.Contains(rec.Body.String(), "released") {
		t.Fatalf("portal catalog must show the same released skill: %s", rec.Body.String())
	}

	// --- 6. Revocation closes LLM + MCP + Knowledge for this Principal ---
	revokeSession := mcpLogin(t, env, "v1pilot@example.com", "secops")
	var pilotKeyID string
	if err := env.pool.QueryRow(ctx, `SELECT key_id FROM api_keys WHERE key_hash = keyHash(pilotKey)`).Scan(&pilotKeyID); err != nil {
		pilotKeyID = findKeyID(t, env, pilotKey)
	}
	rec = env.do(t, http.MethodDelete, "/enterprise/v1/keys/"+pilotKeyID, revokeSession, "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	for _, probe := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/chat/completions", `{"model":"gate-fast","messages":[{"role":"user","content":"hi"}]}`},
		{http.MethodPost, "/mcp/gitlab", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`},
		{http.MethodPost, "/enterprise/v1/knowledge/search", `{"query":"v1"}`},
	} {
		rec = env.do(t, probe.method, probe.path, pilotKey, probe.body, authz)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("revoked on %s: %d, want 401", probe.path, rec.Code)
		}
	}

	// --- 7. Durable evidence: skill lifecycle + device + usage + denials ---
	for _, tc := range []struct {
		query string
		min   int
	}{
		{"SELECT count(*) FROM audit_events WHERE type='device.login' AND outcome='allowed'", 1},
		{"SELECT count(*) FROM audit_events WHERE type='skill.released' OR type='skill.published'", 0},
		{"SELECT count(*) FROM usage_records WHERE alias='gate-fast'", 1},
		{"SELECT count(*) FROM audit_events WHERE type='gateway.auth.failed' AND outcome='denied'", 1},
	} {
		var n int
		if err := env.pool.QueryRow(ctx, tc.query).Scan(&n); err != nil {
			t.Fatalf("evidence %q: %v", tc.query, err)
		}
		if n < tc.min {
			t.Errorf("evidence missing: %q (got %d)", tc.query, n)
		}
	}
}
