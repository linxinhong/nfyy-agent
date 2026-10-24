//go:build integration

// Reversible DLP tokenization coverage (TASK-028): a tokenize-class value
// becomes [rule:token] on every provider-facing surface (the upstream stub
// captures the request body and asserts the original never left), knowledge
// ingest stores the pseudonym, search returns it, tokens are deterministic
// per value, and reveal restores originals only inside the owning workspace
// with audited calls.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
)

func TestDLPTokenizationVault(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	// Upstream provider stub captures every request body.
	var upstreamBodies atomic.Value // []string
	upstreamBodies.Store([]string{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 1<<20)
		n, _ := r.Body.Read(body)
		all, _ := upstreamBodies.Load().([]string)
		upstreamBodies.Store(append(all, string(body[:n])))
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gate-fast","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('tok-gate', $1, 'env:TOK_GATE_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Setenv("TOK_GATE_KEY", "tok-key")
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('gate-fast') ON CONFLICT (alias) DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority, weight) VALUES ('gate-fast','tok-gate','gate-fast',100,1) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('gate-fast','tok-gate',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
		// internal_phone becomes reversible.
		`UPDATE dlp_rules SET action='tokenize' WHERE name='internal_phone'`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `UPDATE dlp_rules SET action='redact' WHERE name='internal_phone';
			DELETE FROM model_routes WHERE alias='gate-fast'; DELETE FROM model_pricing WHERE alias='gate-fast';
			DELETE FROM model_aliases WHERE alias='gate-fast'; DELETE FROM providers WHERE name='tok-gate';
			DELETE FROM dlp_tokens;`)
	})

	scanner := dlp.NewTableScanner(env.store.LoadDLPRules)
	scanner.Tokenizer = func(c context.Context, ruleName, value string) (string, error) {
		// The gateway handlers bind the workspace via withDLPWorkspace; the
		// vault scope follows the request context.
		return env.store.TokenizeDLP(c, "secops", ruleName, value)
	}
	env.srv.Knowledge = newKnowledgeService(t, env)
	env.srv.DLP = scanner
	env.srv.Policy = &policy.TablePolicy{LoadRules: env.store.LoadPolicyRules}
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "tok-user@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "tok-user@example.com"))
	authz := map[string]string{"X-Agent-Workspace": "secops"}

	// 1. LLM path: phone in the prompt never reaches the upstream body.
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"gate-fast","messages":[{"role":"user","content":"给 13812345678 发短信说上线了"}]}`, authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
	}
	bodies, _ := upstreamBodies.Load().([]string)
	joined := strings.Join(bodies, "\n")
	if strings.Contains(joined, "13812345678") {
		t.Fatalf("UPSTREAM LEAK: original phone reached the provider body: %s", joined)
	}
	if !strings.Contains(joined, "[internal_phone:") {
		t.Fatalf("upstream body must carry the pseudonym: %s", joined)
	}
	// Extract the token for later assertions.
	tokenStart := strings.Index(joined, "[internal_phone:") + len("[internal_phone:")
	tokenEnd := strings.Index(joined[tokenStart:], "]")
	token := joined[tokenStart : tokenStart+tokenEnd]

	// 2. Knowledge ingest stores the pseudonym; retrieval returns it.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/documents", keyValue,
		`{"title":"值班表","content":"值班人张三 13812345678 值周一。"}`, authz)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"redactions":1`) {
		t.Fatalf("tokenize upload: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/knowledge/search", keyValue,
		`{"query":"值班表 张三","top_k":3}`, authz)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "13812345678") {
		t.Fatalf("retrieval must return the pseudonym: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[internal_phone:"+token+"]") {
		t.Fatalf("deterministic token expected %s in: %s", token, rec.Body.String())
	}

	// 3. Reveal: same workspace restores; foreign workspace denied.
	portalSession := mcpLogin(t, env, "tok-user@example.com", "secops")
	rec = env.do(t, http.MethodPost, "/enterprise/v1/dlp/reveal", "",
		`{"tokens":["`+token+`"]}`, map[string]string{
			"Authorization": "Bearer " + portalSession, "X-Agent-Workspace": "secops"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"13812345678"`) {
		t.Fatalf("reveal same workspace: %d %s", rec.Code, rec.Body.String())
	}
	finLogin := mcpLogin(t, env, "tok-fin@example.com", "finance")
	rec = env.do(t, http.MethodPost, "/enterprise/v1/dlp/reveal", "",
		`{"tokens":["`+token+`"]}`, map[string]string{
			"Authorization": "Bearer " + finLogin, "X-Agent-Workspace": "finance"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "13812345678") {
		t.Fatalf("foreign workspace reveal must miss: %d %s", rec.Code, rec.Body.String())
	}

	// 4. Reveal audited.
	var n int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type='dlp.reveal' AND outcome='allowed'`).Scan(&n); err != nil || n < 1 {
		t.Fatalf("reveal audit missing: %d %v", n, err)
	}
	_ = time.Second
	_ = json.Valid
}
