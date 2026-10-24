//go:build integration

// TASK-037 acceptance: a multi-match tokenize scan resolves its unique
// values through ONE batched vault exchange (both first-sight insert and
// warm lookup paths), and a released response re-substitutes all
// pseudonyms with ONE batched reveal. Fail-closed behavior is unchanged:
// unknown or foreign tokens stay pseudonyms.
package main_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/store"
)

func TestDLPVaultBatchedAccess(t *testing.T) {
	env := newSliceEnv(t, 1<<30)
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	var upstreamBodies atomic.Value // []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies, _ := upstreamBodies.Load().([]string)
		upstreamBodies.Store(append(bodies, string(body)))
		// The provider echoes pseudonyms back; 'unknown1token' is
		// deliberately absent from the vault.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"call [internal_phone:btch0001aaaa] or [internal_phone:btch0001aaaa] then [internal_phone:btch0002bbbb] plus [internal_phone:unknown1token]"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("BATCH_GATE_KEY", "batch-key")
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('batch-gate', $1, 'env:BATCH_GATE_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('batch-fast') ON CONFLICT DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority) VALUES ('batch-fast','batch-gate','batch-fast',100) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('batch-fast','batch-gate',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
		`UPDATE dlp_rules SET action='tokenize', except_roles='{privileged}' WHERE name='internal_phone'`,
		// Vault rows the echoed response references; 'unknown1token' stays
		// absent on purpose (fail-closed probe).
		`INSERT INTO dlp_tokens (workspace_id, rule_name, token, original_value) VALUES
			('secops','internal_phone','btch0001aaaa','13812340001'),
			('secops','internal_phone','btch0002bbbb','13812340002') ON CONFLICT DO NOTHING`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `UPDATE dlp_rules SET except_roles='{}', action='redact' WHERE name='internal_phone';
			DELETE FROM model_routes WHERE alias='batch-fast'; DELETE FROM model_pricing WHERE alias='batch-fast';
			DELETE FROM model_aliases WHERE alias='batch-fast'; DELETE FROM providers WHERE name='batch-gate'; DELETE FROM dlp_tokens;`)
	})

	// Production wiring with counting batch seams.
	var batchTokenizeCalls, batchedValues, batchRevealCalls atomic.Int64
	scanner := dlp.NewTableScanner(env.store.LoadDLPRules)
	scanner.CacheTTL = 20 * time.Millisecond
	scanner.Tokenizer = func(c context.Context, ruleName, value string) (string, error) {
		t.Errorf("per-match tokenizer must not run when the batch seam is wired")
		return "", nil
	}
	scanner.TokenizeBatch = func(c context.Context, ruleName string, values []string) (map[string]string, error) {
		batchTokenizeCalls.Add(1)
		batchedValues.Add(int64(len(values)))
		return env.store.BatchTokenizeDLP(c, "secops", ruleName, values)
	}
	env.srv.DLP = scanner
	env.srv.Policy = &policy.TablePolicy{LoadRules: env.store.LoadPolicyRules}
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.srv.BatchReveal = func(c context.Context, workspaceID string, tokens []string) (map[string]store.DLPReveal, error) {
		batchRevealCalls.Add(1)
		return env.store.BatchRevealDLP(c, workspaceID, tokens)
	}
	env.handler = env.srv.Handler()

	setupUser := func(email, roles string) string {
		mcpLogin(t, env, email, "secops")
		uid := mcpUserID(t, env, email)
		if _, err := env.pool.Exec(ctx,
			`UPDATE workspace_members SET roles = $1 WHERE user_id = $2`, roles, uid); err != nil {
			t.Fatalf("set roles: %v", err)
		}
		return seedKey(t, env, uid)
	}
	privKey := setupUser("batch-priv@example.com", "{privileged,developer}")
	ordKey := setupUser("batch-ord@example.com", "{developer}")

	// Phase 1: one scan, four unique phones repeated twelve times — the
	// batch seam must fire once per scan with the four unique values. The
	// second request walks the warm (SELECT-only) path of the same seam and
	// must produce identical stable pseudonyms upstream.
	payload := `{"model":"batch-fast","messages":[{"role":"user","content":"` +
		strings.Repeat("call 13812340001, 13812340002, 13812340003, 13812340004. ", 3) + `"}]}`
	for i := 0; i < 2; i++ {
		if rec := env.do(t, http.MethodPost, "/v1/chat/completions", privKey, payload,
			map[string]string{"X-Agent-Workspace": "secops"}); rec.Code != http.StatusOK {
			t.Fatalf("tokenize chat %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if got := batchTokenizeCalls.Load(); got != 2 {
		t.Fatalf("two scans must resolve through one batch call each: %d", got)
	}
	if got := batchedValues.Load(); got != 8 { // 4 unique values x 2 scans
		t.Fatalf("batch must receive unique values only: %d", got)
	}
	bodies := upstreamBodies.Load().([]string)
	if len(bodies) != 2 || !strings.Contains(bodies[0], "[internal_phone:") || bodies[0] != bodies[1] {
		t.Fatalf("upstream must see identical stable pseudonyms on both requests: %q vs %q", bodies[0], bodies[1])
	}

	// Phase 2: the provider response carries three pseudonyms (one repeated,
	// one unknown). The privileged release re-substitutes with exactly one
	// batched reveal; the ordinary user's release never reveals.
	before := batchRevealCalls.Load()
	privOut := env.do(t, http.MethodPost, "/v1/chat/completions", privKey,
		`{"model":"batch-fast","messages":[{"role":"user","content":"summarize"}]}`,
		map[string]string{"X-Agent-Workspace": "secops"})
	if privOut.Code != http.StatusOK {
		t.Fatalf("priv chat: %d", privOut.Code)
	}
	if got := batchRevealCalls.Load() - before; got != 1 {
		t.Fatalf("one released response must issue one batched reveal: %d", got)
	}
	body := privOut.Body.String()
	if !strings.Contains(body, "13812340001") || !strings.Contains(body, "13812340002") {
		t.Fatalf("exempt response must show originals: %s", body)
	}
	if strings.Contains(body, "btch0001aaaa") || strings.Contains(body, "btch0002bbbb") {
		t.Fatalf("no vaulted pseudonym may survive an exempt release: %s", body)
	}
	if !strings.Contains(body, "unknown1token") {
		t.Fatalf("unknown token must stay untouched (fail closed): %s", body)
	}

	ordOut := env.do(t, http.MethodPost, "/v1/chat/completions", ordKey,
		`{"model":"batch-fast","messages":[{"role":"user","content":"summarize"}]}`,
		map[string]string{"X-Agent-Workspace": "secops"})
	if ordOut.Code != http.StatusOK {
		t.Fatalf("ord chat: %d", ordOut.Code)
	}
	if got := batchRevealCalls.Load() - before; got != 1 {
		t.Fatalf("non-exempt release must not reveal: %d", got)
	}
	if !strings.Contains(ordOut.Body.String(), "[internal_phone:") {
		t.Fatalf("ordinary user must keep pseudonyms: %s", ordOut.Body.String())
	}
}
