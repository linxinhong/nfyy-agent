//go:build integration

// DLP alert mode + rule management coverage (TASK-026): alert rules let
// matching content pass on the LLM path while writing dlp.hit (outcome
// allowed, alert_only); deny rules still block; the admin rule endpoints
// list/toggle with fail-closed role checks, audit every change and bump the
// version; non-admins are 403 on both endpoints.

package main_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
)

func TestDLPRuleManagementAndAlertMode(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.Policy = &policy.TablePolicy{LoadRules: env.store.LoadPolicyRules}
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.DLP.(*dlp.TableScanner).CacheTTL = 20 * time.Millisecond // near-live rule propagation

	// Local LLM provider stub so the routed chat path executes end to end.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gate-fast","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO providers (name, base_url, credential_ref) VALUES ('dlp-gate', $1, 'env:DLP_GATE_KEY')
		ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Setenv("DLP_GATE_KEY", "dlp-gate-key")
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('gate-fast') ON CONFLICT (alias) DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority, weight) VALUES ('gate-fast','dlp-gate','gate-fast',100,1) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('gate-fast','dlp-gate',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed llm control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM model_routes WHERE alias='gate-fast';
			DELETE FROM model_pricing WHERE alias='gate-fast';
			DELETE FROM model_aliases WHERE alias='gate-fast';
			DELETE FROM providers WHERE name='dlp-gate';`)
	})

	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.handler = env.srv.Handler()

	adminSession := mcpLogin(t, env, "dlp-admin@example.com", "secops")
	adminID := mcpUserID(t, env, "dlp-admin@example.com")
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspace_members SET roles = '{admin}' WHERE user_id = $1`, adminID); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	adminAuth := map[string]string{"Authorization": "Bearer " + adminSession}
	empSession := mcpLogin(t, env, "dlp-emp@example.com", "secops")
	empAuth := map[string]string{"Authorization": "Bearer " + empSession}

	keyValue := seedKey(t, env, mcpUserID(t, env, "dlp-admin@example.com"))

	// 1. List rules (admin ok, employee 403, anonymous 401).
	rec := env.do(t, http.MethodGet, "/enterprise/v1/admin/dlp/rules", "", "", adminAuth)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"openai_key"`) {
		t.Fatalf("rules list: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, "/enterprise/v1/admin/dlp/rules", "", "", empAuth)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin rules list: %d, want 403", rec.Code)
	}
	rec = env.do(t, http.MethodGet, "/enterprise/v1/admin/dlp/rules", "", "", nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("unauthenticated rules list must not succeed")
	}

	// 2. Toggle openai_key to alert mode (audited, version bumped).
	rec = env.do(t, http.MethodPatch, "/enterprise/v1/admin/dlp/rules/openai_key", "",
		`{"action":"alert"}`, adminAuth)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"action":"alert"`) {
		t.Fatalf("toggle to alert: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPatch, "/enterprise/v1/admin/dlp/rules/openai_key", "",
		`{"action":"bogus"}`, adminAuth)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid action: %d, want 400", rec.Code)
	}
	rec = env.do(t, http.MethodPatch, "/enterprise/v1/admin/dlp/rules/openai_key", "",
		`{"action":"alert"}`, empAuth)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin toggle: %d, want 403", rec.Code)
	}

	// 3. Alert mode end-to-end: content matching the rule now PASSES the LLM
	//    path (real request shape) and the hit is audited as alert-only.
	chat := func(content string) int {
		return env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
			`{"model":"gate-fast","messages":[{"role":"user","content":"`+content+`"}]}`,
			map[string]string{"X-Agent-Workspace": "secops"}).Code
	}
	time.Sleep(40 * time.Millisecond) // scanner cache TTL
	if code := chat("call the api with sk-abcdefghijklmnopqrst1234 please"); code != http.StatusOK {
		t.Fatalf("alert-mode chat must pass: %d", code)
	}
	var alerted int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type='dlp.hit' AND outcome='allowed'
		 AND detail->>'alert_only' = 'true' AND detail->'rules' ? 'openai_key'`).Scan(&alerted); err != nil || alerted < 1 {
		t.Fatalf("alert hit audit missing: %d %v", alerted, err)
	}

	// 4. Back to deny: the same content blocks again (422, zero egress).
	rec = env.do(t, http.MethodPatch, "/enterprise/v1/admin/dlp/rules/openai_key", "",
		`{"action":"deny"}`, adminAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle back to deny: %d", rec.Code)
	}
	time.Sleep(40 * time.Millisecond)
	if code := chat("call the api with sk-abcdefghijklmnopqrst1234 please"); code != http.StatusUnprocessableEntity {
		t.Fatalf("deny-mode chat must block: %d", code)
	}

	// 5. Every change produced a reliable admin audit record.
	var changes int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type='admin.dlp_rule_updated' AND actor_user_id = $1`,
		adminID).Scan(&changes); err != nil || changes < 2 {
		t.Fatalf("admin.dlp_rule_updated audit records missing: %d %v", changes, err)
	}
}
