//go:build integration

// Gateway performance gate (TASK-033, PRD §9): measures the gateway's added
// latency with the FULL V1 governance chain enabled — real middleware, real
// PostgreSQL/Redis, DLP tokenize + exempt re-substitution, policy, reliable
// audit — against a stub provider (provider inference is excluded from the
// "gateway overhead" definition). Emits a per-stage P50/P95/P99 table and a
// hard PASS/FAIL on the PRD budget: gateway overhead P95 < 100ms.

package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/store"

	gateway "enterprise-agent-platform/apps/gateway"
)

type perfStage struct {
	Name    string
	Samples []float64 // ms
}

func (s *perfStage) add(ms float64) { s.Samples = append(s.Samples, ms) }

func (s *perfStage) pct(p float64) float64 {
	if len(s.Samples) == 0 {
		return math.NaN()
	}
	xs := append([]float64(nil), s.Samples...)
	sort.Float64s(xs)
	idx := int(math.Ceil(p/100*float64(len(xs)))) - 1
	if idx < 0 {
		idx = 0
	}
	return xs[idx]
}

func TestGatewayPerformanceGate(t *testing.T) {
	if testing.Short() {
		t.Skip("perf gate needs the full loop")
	}
	env := newSliceEnv(t, 1<<30) // effectively no rate limit for the burst
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	// Stub provider: fixed latency floor of 1ms so the LLM stage stays
	// visible but tiny; we subtract nothing — the harness reports per-stage
	// internals plus end-to-end, and the PRD budget applies to gateway-added
	// stages only (auth+policy+DLP+audit+token-exchange), measured directly.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gate-fast","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`))
	}))
	t.Cleanup(upstream.Close)

	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('perf-gate', $1, 'env:PERF_GATE_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Setenv("PERF_GATE_KEY", "perf-key")
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('gate-fast') ON CONFLICT (alias) DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority, weight) VALUES ('gate-fast','perf-gate','gate-fast',100,1) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('gate-fast','perf-gate',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
		`UPDATE dlp_rules SET action='tokenize', except_roles='{privileged}' WHERE name='internal_phone'`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `UPDATE dlp_rules SET except_roles='{}', action='redact' WHERE name='internal_phone';
			DELETE FROM model_routes WHERE alias='gate-fast'; DELETE FROM model_pricing WHERE alias='gate-fast';
			DELETE FROM model_aliases WHERE alias='gate-fast'; DELETE FROM providers WHERE name='perf-gate'; DELETE FROM dlp_tokens;`)
	})

	scanner := dlp.NewTableScanner(env.store.LoadDLPRules)
	scanner.CacheTTL = time.Minute // production cache
	scanner.Tokenizer = func(c context.Context, ruleName, value string) (string, error) {
		return env.store.TokenizeDLP(c, "secops", ruleName, value)
	}
	// Production batch seam (TASK-037): the gate must measure the batched
	// vault exchange, not the per-match fallback.
	scanner.TokenizeBatch = func(c context.Context, ruleName string, values []string) (map[string]string, error) {
		return env.store.BatchTokenizeDLP(c, "secops", ruleName, values)
	}
	env.srv.DLP = scanner
	// Production control-data cache wiring (TASK-036): the gate must measure
	// what production serves, cached control reads included.
	caches := gateway.NewControlCaches(gateway.ControlCacheTTL, gateway.ControlSourcesFromStore(env.store))
	env.srv.Policy = &policy.TablePolicy{LoadRules: caches.PolicyRules()}
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   caches.ResolvePlan(),
		ResolveTarget: caches.ResolveTarget(),
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.srv.Pricing = caches.PricingKnown()
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "perf-user@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "perf-user@example.com"))

	// --- Stage probes (real internals, not mocks) ---
	phoneMsg := strings.Repeat("给 13812345678 发短信说上线了，", 20)  // ~3.6KB with many phone hits
	bigPayload := strings.Repeat("架构评审会议纪要：", 400) + phoneMsg // ~12KB mixed

	scanStage := &perfStage{Name: "dlp_scan_12kb_tokenize"}
	resubStage := &perfStage{Name: "vault_resub_1token"}
	for i := 0; i < 200; i++ {
		t0 := time.Now()
		res, err := scanner.Scan(ctx, bigPayload)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		scanStage.add(float64(time.Since(t0).Microseconds()) / 1000)
		tok := firstToken(res.Redacted)
		if tok != "" {
			t1 := time.Now()
			n := perfResub(env.store, "secops", "内容 "+tok+" 结尾")
			if n != 1 {
				t.Fatalf("resub expected 1, got %d", n)
			}
			resubStage.add(float64(time.Since(t1).Microseconds()) / 1000)
		}
	}

	// --- End-to-end through the real handler (auth+policy+DLP+audit+LLM) ---
	e2e := &perfStage{Name: "e2e_chat_full_chain"}
	payload, _ := json.Marshal(map[string]any{
		"model": "gate-fast",
		"messages": []map[string]string{
			{"role": "user", "content": bigPayload},
		},
	})
	for i := 0; i < 200; i++ {
		t0 := time.Now()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+keyValue)
		req.Header.Set("X-Agent-Workspace", "secops")
		req.Header.Set("Content-Type", "application/json")
		env.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("chat %d: %d %s", i, rec.Code, rec.Body.String()[:200])
		}
		e2e.add(float64(time.Since(t0).Microseconds()) / 1000)
	}

	// --- Baseline with DLP disabled (engine-off headroom check) ---
	envNoDLP := &perfStage{Name: "e2e_chat_dlp_off"}
	savedDLP := env.srv.DLP
	env.srv.DLP = nil
	// knowledge/llm guards require DLP non-nil: reuse an allow-all scanner instead of nil.
	env.srv.DLP = &allowAllScanner{}
	for i := 0; i < 200; i++ {
		t0 := time.Now()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+keyValue)
		req.Header.Set("X-Agent-Workspace", "secops")
		req.Header.Set("Content-Type", "application/json")
		env.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("baseline %d: %d", i, rec.Code)
		}
		envNoDLP.add(float64(time.Since(t0).Microseconds()) / 1000)
	}
	env.srv.DLP = savedDLP

	// --- SSE window throughput (internal shim, see perf_sse_shim_test.go) ---
	ssePerSec := gateway.SSEThroughputForTest(t, env.srv.DLP)

	// --- Report ---
	stages := []*perfStage{scanStage, resubStage, e2e, envNoDLP}
	var buf strings.Builder
	fmt.Fprintf(&buf, "\n== Gateway performance gate (full governance ON) ==\n")
	for _, st := range stages {
		fmt.Fprintf(&buf, "%-24s n=%d  p50=%6.2fms  p95=%6.2fms  p99=%6.2fms\n",
			st.Name, len(st.Samples), st.pct(50), st.pct(95), st.pct(99))
	}
	dlpOverhead := e2e.pct(95) - envNoDLP.pct(95)
	fmt.Fprintf(&buf, "%-24s        p95 delta (DLP on - off) = %6.2fms\n", "dlp_added_overhead", dlpOverhead)
	fmt.Fprintf(&buf, "%-24s        %d chunks/sec\n", "sse_window", ssePerSec)
	t.Log(buf.String())

	// PRD §9: gateway overhead P95 < 100ms — the gateway-added portion of the
	// full chain. The stub provider contributes ~1ms; the e2e stage includes
	// it, so the budget check uses e2e p95 (a conservative upper bound of
	// gateway-added latency).
	const budgetMs = 100.0
	if p95 := e2e.pct(95); p95 >= budgetMs {
		t.Errorf("PRD §9 VIOLATION: full-chain p95 %.2fms >= %dms budget (gateway-added portion)", p95, int(budgetMs))
	}
	if p95 := dlpOverhead; p95 >= budgetMs {
		t.Errorf("DLP-added p95 %.2fms exceeds budget", p95)
	}
}

func firstToken(text string) string {
	i := strings.Index(text, "[internal_phone:")
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(text[i:], ']')
	if j < 0 {
		return ""
	}
	return text[i : i+j+1]
}

var perfTokenRe = regexp.MustCompile(`\[([A-Za-z0-9_]+):([A-Za-z0-9]{8,32})\]`)

// perfResub mirrors the gateway re-substitution path (one batched reveal,
// as the wired Server.BatchReveal performs since TASK-037).
func perfResub(st *store.Store, workspace, content string) int {
	matches := perfTokenRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return 0
	}
	revealed, err := st.BatchRevealDLP(context.Background(), workspace, uniqueTokens(matches))
	if err != nil {
		return 0
	}
	n := 0
	perfTokenRe.ReplaceAllStringFunc(content, func(m string) string {
		g := perfTokenRe.FindStringSubmatch(m)
		if len(g) != 3 {
			return m
		}
		r, ok := revealed[g[2]]
		if !ok || r.RuleName != g[1] {
			return m
		}
		n++
		return r.Value
	})
	return n
}

func uniqueTokens(matches [][]string) []string {
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, g := range matches {
		if _, ok := seen[g[2]]; !ok {
			seen[g[2]] = struct{}{}
			out = append(out, g[2])
		}
	}
	return out
}

type allowAllScanner struct{}

func (allowAllScanner) Scan(_ context.Context, text string) (llmkernel.DLPResult, error) {
	return llmkernel.DLPResult{Redacted: text}, nil
}
