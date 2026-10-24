//go:build integration

// TASK-046 acceptance: the failure-mode & degradation matrix, proven at the
// system level against the REAL stack (real PostgreSQL/Redis, real handler
// chain, provider stubbed). Every external dependency failing mid-flight
// produces the contracted behavior:
//
//  1. PG unavailable        -> requests fail CLOSED with a DISTINCT
//     storage-error class (HTTP 500 / "credential lookup failed"), never the
//     unknown-key 401; the security-audit delivery failure is logged loudly.
//  2. Redis unavailable     -> rate limiting fails OPEN by contract; every
//     other surface keeps working; the failure is logged.
//  3. Provider failover exhausted -> a deterministic EAP-wrapped
//     upstream_unavailable error with NO provider credential or URL material
//     in the error body; both failed attempts are recorded in audit.
//  4. Provider dies MID-STREAM    -> the client sees a terminal SSE error
//     frame; the audit trail records the failure; the usage ledger never
//     fabricates usage for a route that never completed (ADR-0027: on a
//     routed error the audit set carries the outcome).
//  5. Audit delivery failure      -> the answer is withheld ("request could
//     not be audited") — fail closed, never an unauditable release.
//
// Faults are injected at the infrastructure boundary ONLY (docker
// stop/start of THIS task's qualification containers, failing provider
// stubs, a failing audit sink) — the gateway's own layers are never mocked.
//
// After every scenario the harness drains and asserts stability: no
// goroutine growth and a settled pgxpool (no acquired conns stuck, no
// total-connection growth) — a leak is a GROWING count; a bounded backlog
// that drains is fine (same discipline as the concurrency gate).
//
// ISOLATION: this suite stops/starts its own containers
// (eap-verify-pg-046 / eap-verify-redis-046 on ports 15433/16380). It
// REFUSES to run against any other database/Redis address so the shared dev
// stack can never be fault-injected by accident.
package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	gateway "enterprise-agent-platform/apps/gateway"
	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/governance"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/store"
)

// Qualification container names and ports belong to TASK-046 ONLY. The suite
// fault-injects by stopping/starting these containers; the name+port guard
// below is what keeps the shared dev stack (15432/16379, eap-dev-*) and the
// sibling task containers (15434+/16381+) out of reach.
const (
	fmPGContainer    = "eap-verify-pg-046"
	fmRedisContainer = "eap-verify-redis-046"
	fmPGPort         = "15433"
	fmRedisPort      = "16380"
	// fmProviderCredential is the provider secret used by the seeded stub
	// routes; the leakage assertions prove it never reaches an error body.
	fmProviderCredential = "fm-live-provider-secret-046"
	fmCredentialEnv      = "FM_PROVIDER_CREDENTIAL_046"
)

// fmRequireFaultInfra refuses to run unless the env points at THIS task's
// qualification containers. Without the guard, a misconfigured run could
// stop a container the test database does not live on (dishonest scenario)
// or — worse — let a future edit fault-inject the shared stack.
func fmRequireFaultInfra(t *testing.T) {
	t.Helper()
	if dbURL := os.Getenv("EAP_TEST_DATABASE_URL"); !strings.Contains(dbURL, "127.0.0.1:"+fmPGPort+"/") {
		t.Skipf("PG fault injection requires the TASK-046 qualification container (port %s), got %q — refusing to fault-inject anything else", fmPGPort, dbURL)
	}
	if addr := os.Getenv("EAP_TEST_REDIS_ADDR"); addr != "127.0.0.1:"+fmRedisPort {
		t.Skipf("Redis fault injection requires the TASK-046 qualification container (%s), got %q — refusing to fault-inject anything else", fmRedisPort, addr)
	}
	for _, name := range []string{fmPGContainer, fmRedisContainer} {
		if fmContainerState(name) == "" {
			t.Fatalf("qualification container %s does not exist (make verify-failure-modes creates it)", name)
		}
	}
}

// --- docker fault injection (infrastructure boundary only) ---

func fmDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out)
}

// fmContainerState returns "running"/"exited"/... or "" when missing.
func fmContainerState(name string) string {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Status}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fmStopContainer(t *testing.T, name string) {
	t.Helper()
	if state := fmContainerState(name); state == "running" {
		fmDocker(t, "stop", "-t", "5", name)
	}
}

func fmStartContainer(t *testing.T, name string) {
	t.Helper()
	if state := fmContainerState(name); state != "" && state != "running" {
		fmDocker(t, "start", name)
	}
}

// fmWaitPGBack polls the pool (pgxpool reconnects by itself) until PostgreSQL
// answers again — recovery is asserted, not slept through.
func fmWaitPGBack(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := pool.Ping(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("PostgreSQL did not come back after container restart: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func fmWaitRedisBack(t *testing.T, addr string) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer func() { _ = client.Close() }()
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := client.Ping(ctx).Err()
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Redis did not come back after container restart: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// --- log capture: the loud-failure contracts are log-observable ---

func fmCaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// --- drain + stability checks (after EVERY scenario) ---

// fmBaseline captures the pre-scenario goroutine count and pool totals after
// a warmup request so lazy startup goroutines are not counted as leaks.
type fmBaseline struct {
	goroutines int
	totalConns int32
}

func fmCaptureBaseline(pool *pgxpool.Pool) fmBaseline {
	return fmBaseline{
		goroutines: runtime.NumGoroutine(),
		totalConns: pool.Stat().TotalConns(),
	}
}

// fmAssertDrained samples until the process settles (bounded poll, not a
// blind sleep) and asserts: goroutines did not GROW past the tolerance, no
// pool connections are still acquired, and the pool did not grow.
func fmAssertDrained(t *testing.T, label string, before fmBaseline, pool *pgxpool.Pool) {
	t.Helper()

	// Drain: sample goroutines until a sample is no larger than the previous
	// one twice in a row, or a bounded deadline passes.
	deadline := time.Now().Add(8 * time.Second)
	prev := runtime.NumGoroutine()
	settled := 0
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		cur := runtime.NumGoroutine()
		if cur <= prev {
			settled++
			if settled >= 2 {
				break
			}
		} else {
			settled = 0
		}
		prev = cur
	}
	if prev > before.goroutines+10 {
		t.Errorf("[%s] goroutine leak: baseline=%d settled=%d", label, before.goroutines, prev)
	}

	// Pool: no checkouts may survive the drain.
	statDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(statDeadline) {
		if pool.Stat().AcquiredConns() == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("[%s] pgxpool leak: %d connections still acquired after drain", label, n)
	}
	if total := pool.Stat().TotalConns(); total > before.totalConns+4 {
		t.Errorf("[%s] pgxpool growth: baseline=%d settled=%d", label, before.totalConns, total)
	}
}

// --- chat-stack wiring (production governance chain, per durable path) ---

// fmWireGovernance mirrors the production assembly in main.go: cached control
// reads, table policy, DLP table scanner, routed LLM.
func fmWireGovernance(t *testing.T, env *sliceEnv) {
	t.Helper()
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
}

// fmSeedAlias seeds a logical alias routed across the named providers in
// priority order, all sharing one credential reference (env-resolved, so the
// leakage assertions have a concrete secret to hunt for).
func fmSeedAlias(t *testing.T, env *sliceEnv, alias string, orderedNames []string, providerURLs map[string]string) {
	t.Helper()
	t.Setenv(fmCredentialEnv, fmProviderCredential)
	ctx := context.Background()
	for _, name := range orderedNames {
		if _, err := env.pool.Exec(ctx,
			`INSERT INTO providers (name, base_url, credential_ref) VALUES ($1, $2, $3)
			 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`,
			name, providerURLs[name], "env:"+fmCredentialEnv); err != nil {
			t.Fatalf("seed provider %s: %v", name, err)
		}
	}
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO model_aliases (alias) VALUES ($1) ON CONFLICT DO NOTHING`, alias); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	for i, name := range orderedNames {
		// The route's model doubles as the provider-native name here; the
		// stubs accept anything and the scenarios never depend on it.
		if _, err := env.pool.Exec(ctx,
			`INSERT INTO model_routes (alias, provider, model, priority) VALUES ($1, $2, $1, $3) ON CONFLICT DO NOTHING`,
			alias, name, 100*(i+1)); err != nil {
			t.Fatalf("seed route %s: %v", name, err)
		}
		if _, err := env.pool.Exec(ctx,
			`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ($1, $2, 1.0, 2.0)
			 ON CONFLICT (alias, provider) DO NOTHING`, alias, name); err != nil {
			t.Fatalf("seed pricing %s: %v", name, err)
		}
	}
}

func fmUnique(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func fmCount(t *testing.T, env *sliceEnv, query string, args ...any) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// fmMe sends one /enterprise/v1/me request with its own bounded context (a
// dependency outage must surface as a fast fail-closed error, never a hang;
// the deadline makes a hang assertable instead of deadlocking the suite).
func fmMe(t *testing.T, env *sliceEnv, key, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/enterprise/v1/me", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+key)
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Scenario 3: provider failover exhausted (deterministic EAP-wrapped error).
// ---------------------------------------------------------------------------

func TestFailureModesProviderFailoverExhausted(t *testing.T) {
	env := newSliceEnv(t, 1<<30)
	env.srv.OIDCStore = env.store
	fmWireGovernance(t, env)

	// Two routes, both failing at the infrastructure boundary: the primary
	// accepts then answers 500; the backup kills the connection before any
	// byte. Two DIFFERENT failure shapes must converge on ONE EAP class.
	primaryHits, backupHits := 0, 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits++
		panic(http.ErrAbortHandler) // conn kill: the client sees EOF, no response
	}))
	t.Cleanup(backup.Close)

	fmSeedAlias(t, env, "fm-exhausted",
		[]string{"fm-exh-primary", "fm-exh-backup"},
		map[string]string{"fm-exh-primary": primary.URL, "fm-exh-backup": backup.URL})

	mcpLogin(t, env, "fm-exh@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "fm-exh@example.com"))

	chat := func(id string) *httptest.ResponseRecorder {
		return env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
			`{"model":"fm-exhausted","messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"X-Agent-Workspace": "secops", "X-Request-ID": id})
	}

	// Warmup (the failing path warms the same lazy resources) + baseline.
	if rec := chat(fmUnique("fm-warm")); rec.Code != http.StatusBadGateway {
		t.Fatalf("exhausted failover warmup: %d %s, want 502", rec.Code, rec.Body.String())
	}
	baseline := fmCaptureBaseline(env.pool)

	reqID := fmUnique("fm-exh")
	rec := chat(reqID)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("exhausted failover: %d %s, want 502 upstream_unavailable", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body must be the frozen OpenAI shape: %v", err)
	}
	if body.Error.Code != string(errs.UpstreamUnavailable) {
		t.Fatalf("error code must be the EAP upstream class, got %q (%s)", body.Error.Code, rec.Body.String())
	}
	// No provider credential or URL material may leak into the client body.
	if strings.Contains(rec.Body.String(), fmProviderCredential) {
		t.Fatalf("provider credential leaked into the error body: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "127.0.0.1") ||
		strings.Contains(rec.Body.String(), primary.URL) ||
		strings.Contains(rec.Body.String(), backup.URL) {
		t.Fatalf("provider URL material leaked into the error body: %s", rec.Body.String())
	}

	// Deterministic: a second identical failure yields the same EAP class and
	// message (client retry decisions key on it).
	rec2 := chat(fmUnique("fm-exh"))
	if rec2.Code != rec.Code {
		t.Fatalf("nondeterministic class: %d vs %d", rec2.Code, rec.Code)
	}
	var body2 struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &body2)
	if body2.Error.Message != body.Error.Message || body2.Error.Code != body.Error.Code {
		t.Fatalf("nondeterministic error body: %q/%q vs %q/%q",
			body2.Error.Message, body2.Error.Code, body.Error.Message, body.Error.Code)
	}
	// Both routes were walked, in plan order, on every request (warmup +
	// two scenario requests).
	if primaryHits != 3 || backupHits != 3 {
		t.Fatalf("deterministic failover must walk both routes per request: primary=%d backup=%d", primaryHits, backupHits)
	}

	// Attempts recorded in audit: one llm.request with attempts=2 carrying
	// the upstream error class, plus the policy decision — and NO usage row
	// for a request that never selected a route (ADR-0027).
	if got := fmCount(t, env,
		`SELECT count(*) FROM audit_events WHERE request_id=$1 AND type='llm.request'
		 AND detail->>'attempts'='2' AND detail->>'error_code'='upstream_unavailable' AND outcome='denied'`,
		reqID); got != 1 {
		t.Fatalf("failed attempts must be recorded in audit (attempts=2, error_code=upstream_unavailable), got %d", got)
	}
	if got := fmCount(t, env,
		`SELECT count(*) FROM audit_events WHERE request_id=$1 AND type='policy.decision'`, reqID); got != 1 {
		t.Fatalf("policy decision must be recorded for the failed request, got %d", got)
	}
	if got := fmCount(t, env, `SELECT count(*) FROM usage_records WHERE request_id=$1`, reqID); got != 0 {
		t.Fatalf("no usage row may exist for a request that never completed, got %d", got)
	}

	fmAssertDrained(t, "failover-exhausted", baseline, env.pool)
}

// ---------------------------------------------------------------------------
// Scenario 4: provider dies mid-stream (terminal SSE error frame).
// ---------------------------------------------------------------------------

func TestFailureModesMidStreamProviderDeath(t *testing.T) {
	env := newSliceEnv(t, 1<<30)
	env.srv.OIDCStore = env.store
	fmWireGovernance(t, env)

	// The stub starts a real SSE stream, emits two deltas with enough pacing
	// for the gateway to consume them and park on its select, then kills the
	// connection mid-response (abort, no finish/DONE frames after it).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		delta := func(text string) {
			b, _ := json.Marshal(map[string]any{"id": "s1", "object": "chat.completion.chunk", "model": "fm-mid-model",
				"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": text}}}})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
		delta("hello ")
		time.Sleep(150 * time.Millisecond)
		delta("world")
		time.Sleep(150 * time.Millisecond)
		// Mid-stream death: abort the connection without finish/DONE frames.
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(upstream.Close)

	fmSeedAlias(t, env, "fm-midstream",
		[]string{"fm-mid-provider"},
		map[string]string{"fm-mid-provider": upstream.URL})

	mcpLogin(t, env, "fm-mid@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "fm-mid@example.com"))

	baseline := fmCaptureBaseline(env.pool)

	ts := httptest.NewServer(env.handler)
	t.Cleanup(ts.Close)
	httpClient := &http.Client{Timeout: 30 * time.Second}

	reqID := fmUnique("fm-mid")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"fm-midstream","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+keyValue)
	req.Header.Set("X-Agent-Workspace", "secops")
	req.Header.Set("X-Request-ID", reqID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status: %d (headers must already be committed when the upstream dies)", resp.StatusCode)
	}

	// Read the frames: the client must see a TERMINAL error chunk, then [DONE].
	var sawDone, sawTerminal bool
	terminalContent := ""
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != nil && *c.FinishReason == "error" {
				sawTerminal = true
				terminalContent = c.Delta.Content
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("stream read: %v", err)
	}
	if !sawTerminal {
		t.Fatalf("client must see a terminal SSE error frame when the provider dies mid-stream")
	}
	if !strings.Contains(terminalContent, "[stream error]") {
		t.Fatalf("terminal frame must carry the EAP stream-error marker, got %q", terminalContent)
	}
	if !sawDone {
		t.Fatalf("the error frame must be terminal: [DONE] must close the stream after it")
	}
	// Release the client's own idle keep-alive readers before drain sampling.
	httpClient.CloseIdleConnections()

	// Audit recorded the failure; the ledger holds no fabricated usage for a
	// route that never finished (ADR-0027: on a routed error the audit set
	// carries the outcome).
	if got := fmCount(t, env,
		`SELECT count(*) FROM audit_events WHERE request_id=$1 AND type='llm.request'
		 AND detail->>'error_code'='upstream_unavailable' AND outcome='denied'`, reqID); got != 1 {
		t.Fatalf("mid-stream death must be recorded in audit with the upstream error class, got %d", got)
	}
	if got := fmCount(t, env,
		`SELECT count(*) FROM audit_events WHERE request_id=$1 AND type='policy.decision'`, reqID); got != 1 {
		t.Fatalf("policy decision must be recorded for the failed stream, got %d", got)
	}
	if got := fmCount(t, env, `SELECT count(*) FROM usage_records WHERE request_id=$1`, reqID); got != 0 {
		t.Fatalf("no usage may be billed for a stream that never completed, got %d rows", got)
	}

	fmAssertDrained(t, "mid-stream-death", baseline, env.pool)
}

// ---------------------------------------------------------------------------
// Scenario 5: audit delivery failure withholds the answer (fail closed).
// ---------------------------------------------------------------------------

// fmAuditGateWriter wraps the concrete store as the reliable audit sink and
// can force delivery failures on the production single-commit governance
// batch (ADR-0027). Everything else passes straight through.
type fmAuditGateWriter struct {
	gateway.ControlStore
	storeWriter *store.Store
	forceFail   atomic.Bool
	batchCalls  atomic.Int64
}

func (w *fmAuditGateWriter) WriteGovernanceBatch(ctx context.Context, events []audit.Event, usage *governance.UsageRecord) error {
	w.batchCalls.Add(1)
	if w.forceFail.Load() {
		return errors.New("forced governance batch delivery failure")
	}
	return w.storeWriter.WriteGovernanceBatch(ctx, events, usage)
}

func (w *fmAuditGateWriter) Write(ctx context.Context, e audit.Event) error {
	if w.forceFail.Load() {
		return errors.New("forced audit delivery failure")
	}
	return w.storeWriter.Write(ctx, e)
}

func TestFailureModesAuditDeliveryFailure(t *testing.T) {
	env := newSliceEnv(t, 1<<30)
	env.srv.OIDCStore = env.store
	fmWireGovernance(t, env)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"the answer"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	fmSeedAlias(t, env, "fm-audit",
		[]string{"fm-audit-provider"},
		map[string]string{"fm-audit-provider": upstream.URL})

	gate := &fmAuditGateWriter{ControlStore: env.store, storeWriter: env.store}
	env.srv.AuditWriter = gate // the reliable sink is now the guarded one
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "fm-audit@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "fm-audit@example.com"))

	chat := func(id, content string) *httptest.ResponseRecorder {
		return env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
			fmt.Sprintf(`{"model":"fm-audit","messages":[{"role":"user","content":%q}]}`, content),
			map[string]string{"X-Agent-Workspace": "secops", "X-Request-ID": id})
	}

	// Warmup + baseline with delivery healthy.
	if rec := chat(fmUnique("fm-warm"), "warm"); rec.Code != http.StatusOK {
		t.Fatalf("warmup chat: %d %s", rec.Code, rec.Body.String())
	}
	baseline := fmCaptureBaseline(env.pool)

	// The fault: the reliable sink rejects deliveries.
	gate.forceFail.Store(true)
	reqID := fmUnique("fm-audit-fail")
	rec := chat(reqID, "phone 13812345678")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unauditable request must fail closed: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "could not be audited") {
		t.Fatalf("failure must name the audit gate: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "the answer") {
		t.Fatalf("the answer must be withheld when the request cannot be audited: %s", rec.Body.String())
	}
	// All-or-nothing: the undeliverable batch must leave no usage row either.
	if got := fmCount(t, env, `SELECT count(*) FROM usage_records WHERE request_id=$1`, reqID); got != 0 {
		t.Fatalf("a withheld request must not land in the usage ledger, got %d rows", got)
	}

	// Recovery: with delivery healthy again, the same key is served again.
	gate.forceFail.Store(false)
	okID := fmUnique("fm-audit-ok")
	rec = chat(okID, "hello again")
	if rec.Code != http.StatusOK {
		t.Fatalf("recovered delivery must serve: %d %s", rec.Code, rec.Body.String())
	}
	if got := fmCount(t, env, `SELECT count(*) FROM usage_records WHERE request_id=$1`, okID); got != 1 {
		t.Fatalf("the recovered request must land in the usage ledger exactly once, got %d", got)
	}

	fmAssertDrained(t, "audit-delivery-failure", baseline, env.pool)
}

// ---------------------------------------------------------------------------
// Scenario 2: Redis unavailable -> rate limiting fails OPEN.
// ---------------------------------------------------------------------------

func TestFailureModesRedisRateLimitFailsOpen(t *testing.T) {
	fmRequireFaultInfra(t)
	// Limit=2 makes the limiter observable: the 3rd in-window request 429s.
	env := newSliceEnv(t, 2)

	mcpLogin(t, env, "fm-rl@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "fm-rl@example.com"))

	// Baseline: with Redis healthy the limiter enforces (2x 200, then 429).
	for i := 0; i < 2; i++ {
		if rec := fmMe(t, env, keyValue, fmUnique("fm-rl")); rec.Code != http.StatusOK {
			t.Fatalf("healthy limiter request %d: %d, want 200", i+1, rec.Code)
		}
	}
	if rec := fmMe(t, env, keyValue, fmUnique("fm-rl")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("healthy limiter must enforce the window: %d, want 429", rec.Code)
	}

	baseline := fmCaptureBaseline(env.pool)
	logs := fmCaptureLogs(t)

	// Fault: stop Redis. The limiter must fail OPEN and LOG the failure;
	// everything else (auth chain, membership, audit) keeps working on PG.
	fmStopContainer(t, fmRedisContainer)
	t.Cleanup(func() { fmStartContainer(t, fmRedisContainer) })

	for i := 0; i < 3; i++ {
		rec := fmMe(t, env, keyValue, fmUnique("fm-rl"))
		if rec.Code != http.StatusOK {
			t.Fatalf("rate limiting must fail OPEN while Redis is down: request %d got %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "rate limiter unavailable") {
		t.Fatalf("the limiter failure must be logged loudly, logs: %s", logs.String())
	}

	// Recovery: Redis back, the window enforces again. A graceful Redis
	// shutdown persists an RDB snapshot, so the pre-outage counter can
	// survive the restart — flush it to observe a fresh window and prove the
	// limiter reads Redis again (not stuck in fail-open).
	fmStartContainer(t, fmRedisContainer)
	fmWaitRedisBack(t, "127.0.0.1:"+fmRedisPort)
	fmDocker(t, "exec", fmRedisContainer, "redis-cli", "FLUSHALL")
	for i := 0; i < 2; i++ {
		if rec := fmMe(t, env, keyValue, fmUnique("fm-rl")); rec.Code != http.StatusOK {
			t.Fatalf("recovered limiter request %d: %d, want 200", i+1, rec.Code)
		}
	}
	if rec := fmMe(t, env, keyValue, fmUnique("fm-rl")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after recovery the limiter must enforce again: %d, want 429", rec.Code)
	}

	fmAssertDrained(t, "redis-unavailable", baseline, env.pool)
}

// ---------------------------------------------------------------------------
// Scenario 1: PG unavailable -> fail CLOSED with a DISTINCT storage-error
// class, audit delivery failure logged loudly. Runs LAST: it takes the
// qualification PostgreSQL down and restarts it before returning.
// ---------------------------------------------------------------------------

func TestFailureModesPGUnavailableFailsClosed(t *testing.T) {
	fmRequireFaultInfra(t)
	env := newSliceEnv(t, 1000)

	mcpLogin(t, env, "fm-pg@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "fm-pg@example.com"))

	// Healthy contrast: an UNKNOWN key is an auth denial (401), audited with
	// reason=unknown_key. This is the class the outage must NOT collapse into.
	if rec := fmMe(t, env, "agk_live_definitelynotakey", fmUnique("fm-unknown")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key while healthy: %d, want 401", rec.Code)
	}
	if got := fmCount(t, env,
		`SELECT count(*) FROM audit_events WHERE type='gateway.auth.failed' AND detail->>'reason'='unknown_key'`); got < 1 {
		t.Fatalf("unknown-key denial must be audited as unknown_key, got %d", got)
	}

	// Warmup + baseline, then capture the log stream for the loud-failure
	// assertions.
	if rec := fmMe(t, env, keyValue, fmUnique("fm-pg")); rec.Code != http.StatusOK {
		t.Fatalf("healthy request: %d %s", rec.Code, rec.Body.String())
	}
	baseline := fmCaptureBaseline(env.pool)
	logs := fmCaptureLogs(t)

	// Fault: stop the qualification PostgreSQL container.
	fmStopContainer(t, fmPGContainer)
	t.Cleanup(func() { fmStartContainer(t, fmPGContainer) })

	// The SAME valid key now fails CLOSED with a storage-error class: 500 +
	// "credential lookup failed", never the unknown-key 401.
	rec := fmMe(t, env, keyValue, fmUnique("fm-pg"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PG outage must fail closed with a storage error: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "invalid or unusable API key") {
		t.Fatalf("a storage outage must be DISTINCT from an unknown key: %s", rec.Body.String())
	}
	var problem struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("error body must be the frozen problem shape: %v", err)
	}
	if problem.Code != string(errs.Internal) || !strings.Contains(problem.Message, "credential lookup failed") {
		t.Fatalf("storage-error class wrong: %+v (%s)", problem, rec.Body.String())
	}

	// The security-audit delivery failure for that denial is loud and
	// observable — the denial stands, but the audit gap is not silent.
	if !strings.Contains(logs.String(), "SECURITY AUDIT delivery failure") {
		t.Fatalf("audit delivery failure must be logged loudly during the outage, logs: %s", logs.String())
	}

	// Recovery: container back, the pool reconnects and serves again.
	fmStartContainer(t, fmPGContainer)
	fmWaitPGBack(t, env.pool)
	if rec := fmMe(t, env, keyValue, fmUnique("fm-pg")); rec.Code != http.StatusOK {
		t.Fatalf("recovered stack must serve: %d %s", rec.Code, rec.Body.String())
	}

	fmAssertDrained(t, "pg-unavailable", baseline, env.pool)
}
