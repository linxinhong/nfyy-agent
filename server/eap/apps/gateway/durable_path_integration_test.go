//go:build integration

// TASK-038 acceptance: one routed chat with a dlp.hit delivers its three
// reliable audit events in a single batched round trip; a forced batch
// delivery failure withholds the answer (fail closed); and a revoked key
// is rejected on the very next request — revocation is judged live.
package main_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/store"

	gateway "enterprise-agent-platform/apps/gateway"
)

// batchCountingWriter wraps the store, counts WriteBatch deliveries and can
// be forced to fail them. Everything else passes straight through.
type batchCountingWriter struct {
	gateway.ControlStore
	storeWriter *store.Store // WriteBatch lives on the concrete store
	batchCalls  atomic.Int64
	forceFail   atomic.Bool
}

func (c *batchCountingWriter) WriteBatch(ctx context.Context, events []audit.Event) error {
	c.batchCalls.Add(1)
	if c.forceFail.Load() {
		return errors.New("forced batch delivery failure")
	}
	return c.storeWriter.WriteBatch(ctx, events)
}

func durablePathSetup(t *testing.T) (*sliceEnv, *batchCountingWriter, string) {
	t.Helper()
	env := newSliceEnv(t, 1<<30)
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("DURABLE_GATE_KEY", "durable-key")
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('durable-gate', $1, 'env:DURABLE_GATE_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('durable-fast') ON CONFLICT DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority) VALUES ('durable-fast','durable-gate','durable-fast',100) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('durable-fast','durable-gate',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM model_routes WHERE alias='durable-fast'; DELETE FROM model_pricing WHERE alias='durable-fast';
			DELETE FROM model_aliases WHERE alias='durable-fast'; DELETE FROM providers WHERE name='durable-gate'; DELETE FROM usage_records WHERE alias='durable-fast';`)
	})

	cw := &batchCountingWriter{ControlStore: env.store, storeWriter: env.store}
	env.srv.AuthChain = env.store.GetAPIKeyAuthByHash
	env.srv.AuditWriter = cw
	// Production governance chain (mirrors main.go): cached control reads,
	// table policy, DLP scanner, routed LLM.
	caches := gateway.NewControlCaches(gateway.ControlCacheTTL, gateway.ControlSourcesFromStore(env.store))
	env.srv.Policy = &policy.TablePolicy{LoadRules: caches.PolicyRules()}
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   caches.ResolvePlan(),
		ResolveTarget: caches.ResolveTarget(),
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.srv.Pricing = caches.PricingKnown()
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "durable-user@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "durable-user@example.com"))
	return env, cw, keyValue
}

// chat sends one routed chat tagged with a unique request id.
func durableChat(t *testing.T, env *sliceEnv, keyValue, reqID, content string) *httptest.ResponseRecorder {
	t.Helper()
	return env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		fmt.Sprintf(`{"model":"durable-fast","messages":[{"role":"user","content":%q}]}`, content),
		map[string]string{"X-Agent-Workspace": "secops", "X-Request-ID": reqID})
}

func TestAuditBatchDelivery(t *testing.T) {
	env, cw, keyValue := durablePathSetup(t)
	reqID := "dur-batch-" + gatewayRandomSuffix()

	// One chat carrying a phone -> policy.decision + dlp.hit + llm.request
	// in ONE batched delivery, with all three rows durable afterwards.
	before := cw.batchCalls.Load()
	rec := durableChat(t, env, keyValue, reqID, "phone 13812345678")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
	}
	if got := cw.batchCalls.Load() - before; got != 1 {
		t.Fatalf("three audit events must share one batched delivery: %d", got)
	}
	var kinds int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(DISTINCT type) FROM audit_events
		WHERE request_id = $1 AND type IN ('policy.decision','dlp.hit','llm.request')`,
		reqID).Scan(&kinds); err != nil || kinds != 3 {
		t.Fatalf("all three events must be durable: %d %v", kinds, err)
	}
}

func TestAuditBatchFailureWithholdsAnswer(t *testing.T) {
	env, cw, keyValue := durablePathSetup(t)
	cw.forceFail.Store(true)

	rec := durableChat(t, env, keyValue, "dur-fail-1", "phone 13812345678")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unauditable request must fail closed: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "could not be audited") {
		t.Fatalf("failure must name the audit gate: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("the answer must be withheld: %s", rec.Body.String())
	}

	// Recovery: with delivery working again, the same key chats fine.
	cw.forceFail.Store(false)
	if rec := durableChat(t, env, keyValue, "dur-fail-2", "hello"); rec.Code != http.StatusOK {
		t.Fatalf("recovered delivery must serve: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRevokedKeyRejectedLive(t *testing.T) {
	env, _, keyValue := durablePathSetup(t)
	ctx := context.Background()

	if rec := durableChat(t, env, keyValue, "dur-rev-0", "hi"); rec.Code != http.StatusOK {
		t.Fatalf("pre-revocation chat must pass: %d", rec.Code)
	}

	// Revoke directly in the store — the next request must see it live.
	keyID := ""
	if err := env.pool.QueryRow(ctx,
		`SELECT key_id FROM api_keys WHERE revoked_at IS NULL ORDER BY created_at DESC LIMIT 1`).Scan(&keyID); err != nil {
		t.Fatalf("find key: %v", err)
	}
	if _, err := env.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE key_id = $1`, keyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	rec := durableChat(t, env, keyValue, "dur-rev-1", "hi")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key must be rejected on the next request: %d %s", rec.Code, rec.Body.String())
	}
}

func gatewayRandomSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 16)
}
