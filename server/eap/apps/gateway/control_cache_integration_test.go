//go:build integration

// TASK-036 acceptance (ADR-0025): steady-state routed chat must not re-read
// control data — policy rules, route plan, provider target and pricing load
// once per TTL window no matter how many requests run — and a control-plane
// rule change must take effect within the documented staleness bound.

package main_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"

	"enterprise-agent-platform/packages/store"

	gateway "enterprise-agent-platform/apps/gateway"
)

func cacheTestChat(t *testing.T, env *sliceEnv, keyValue, alias string) *httptest.ResponseRecorder {
	t.Helper()
	return env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"`+alias+`","messages":[{"role":"user","content":"ping the perf stub"}]}`,
		map[string]string{"X-Agent-Workspace": "secops"})
}

// Counting acceptance: 201 sequential routed requests inside one TTL window
// issue exactly one load of each control-data shape.
func TestControlCachesSteadyStateLoadCounts(t *testing.T) {
	env := newSliceEnv(t, 1<<30)
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('cache-probe', $1, 'env:CACHE_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Setenv("CACHE_KEY", "cache-key")
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('cache-fast') ON CONFLICT DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority) VALUES ('cache-fast','cache-probe','fast',100) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('cache-fast','cache-probe',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM model_routes WHERE alias='cache-fast'; DELETE FROM model_pricing WHERE alias='cache-fast';
			DELETE FROM model_aliases WHERE alias='cache-fast'; DELETE FROM providers WHERE name='cache-probe'; DELETE FROM usage_records WHERE alias='cache-fast';`)
	})

	// Production wiring with counting loaders; 10s TTL comfortably spans the
	// request loop, so any reload would be a cache miss, not a window edge.
	var policyLoads, planLoads, targetLoads, pricingLoads atomic.Int64
	src := gateway.ControlSourcesFromStore(env.store)
	src.PolicyRules = func(c context.Context) ([]policy.TableRule, error) {
		policyLoads.Add(1)
		return env.store.LoadPolicyRules(c)
	}
	src.Plan = func(c context.Context, alias string) (llmkernel.RoutePlanView, error) {
		planLoads.Add(1)
		return env.store.RoutePlanFor(c, alias)
	}
	src.Target = func(c context.Context, provider string) (llmkernel.ProviderTarget, error) {
		targetLoads.Add(1)
		return env.store.ResolveProviderTarget(c, provider)
	}
	src.Pricing = func(c context.Context, alias, provider string) (store.PricingLookup, error) {
		pricingLoads.Add(1)
		return env.store.PricingLookupFor(c, alias, provider)
	}
	caches := gateway.NewControlCaches(10*time.Second, src)
	env.srv.Policy = &policy.TablePolicy{LoadRules: caches.PolicyRules()}
	env.srv.DLP = allowAllScanner{}
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   caches.ResolvePlan(),
		ResolveTarget: caches.ResolveTarget(),
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.srv.Pricing = caches.PricingKnown()
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "cache-user@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "cache-user@example.com"))

	const requests = 201
	for i := 0; i < requests; i++ {
		if rec := cacheTestChat(t, env, keyValue, "cache-fast"); rec.Code != http.StatusOK {
			t.Fatalf("chat %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	for name, counter := range map[string]*atomic.Int64{
		"policy_rules":    &policyLoads,
		"route_plan":      &planLoads,
		"provider_target": &targetLoads,
		"pricing":         &pricingLoads,
	} {
		if got := counter.Load(); got != 1 {
			t.Fatalf("%s loaded %d times for %d requests; steady state must load once per TTL window", name, got, requests)
		}
	}
}

// Propagation acceptance: a control-plane policy write takes effect within
// the documented bound (one TTL), not "never".
func TestControlCacheRuleChangePropagatesWithinBound(t *testing.T) {
	env := newSliceEnv(t, 1<<30)
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("CACHE_KEY", "cache-key")
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('cache-prop', $1, 'env:CACHE_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('cache-prop-fast') ON CONFLICT DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority) VALUES ('cache-prop-fast','cache-prop','fast',100) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('cache-prop-fast','cache-prop',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
		`INSERT INTO policy_rules (rule_id, workspace_id, model_alias, effect, priority) VALUES
			('cache-prop-rule', 'secops', 'cache-prop-fast', 'allow', 50)
		 ON CONFLICT (rule_id) DO UPDATE SET effect = EXCLUDED.effect`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM policy_rules WHERE rule_id='cache-prop-rule'; DELETE FROM model_routes WHERE alias='cache-prop-fast';
			DELETE FROM model_pricing WHERE alias='cache-prop-fast'; DELETE FROM model_aliases WHERE alias='cache-prop-fast';
			DELETE FROM providers WHERE name='cache-prop'; DELETE FROM usage_records WHERE alias='cache-prop-fast';`)
	})

	// Short documented-bound stand-in: the production bound is 60s; the
	// semantics under test (write -> visible within one TTL) are identical.
	caches := gateway.NewControlCaches(40*time.Millisecond, gateway.ControlSourcesFromStore(env.store))
	env.srv.Policy = &policy.TablePolicy{LoadRules: caches.PolicyRules()}
	env.srv.DLP = allowAllScanner{}
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   caches.ResolvePlan(),
		ResolveTarget: caches.ResolveTarget(),
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.srv.Pricing = caches.PricingKnown()
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "cache-prop-user@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "cache-prop-user@example.com"))

	if rec := cacheTestChat(t, env, keyValue, "cache-prop-fast"); rec.Code != http.StatusOK {
		t.Fatalf("allowed rule must pass through: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := env.pool.Exec(ctx,
		`UPDATE policy_rules SET effect='deny' WHERE rule_id='cache-prop-rule'`); err != nil {
		t.Fatalf("flip rule: %v", err)
	}
	// Bound = one TTL (+ scheduler slack): the denial must surface now.
	time.Sleep(150 * time.Millisecond)
	rec := cacheTestChat(t, env, keyValue, "cache-prop-fast")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("deny rule must take effect within the staleness bound: %d %s", rec.Code, rec.Body.String()[:200])
	}
}
