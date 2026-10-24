//go:build integration

// TASK-049 (soak): the concurrency gate (TASK-041) proves capacity in bursts;
// leaks that only surface after minutes of pressure need a sustained run.
// This harness drives 200 in-flight users at 200ms agent-like pacing with the
// FULL governance chain ON — DLP tokenize, policy, the batched audit/usage
// commit, routed LLM against a ~1ms stub provider — for SOAK_DURATION
// (default 30m), sampling runtime and infrastructure resources every 10s and
// issuing a hard stability verdict: no monotonic growth beyond noise in
// goroutines/heap/PG-pool/Redis/open-FD series, whole-run p95 inside the PRD
// §9 budget, zero non-200s, a clean post-run drain, and no audit/usage
// backlog. The process is never restarted mid-run; a drift finding here is a
// card-level escalation, not something the harness may paper over.

package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	gateway "enterprise-agent-platform/apps/gateway"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
)

// soakSample is one periodic resource observation. The DB row counts double as
// the audit/usage backlog probe: the governance batch commits synchronously
// per request, so usage rows must track completed requests exactly — a gap
// means either a lossy write path or an async backlog piling up.
type soakSample struct {
	at           time.Duration
	goroutines   int
	heapAllocMB  float64
	heapObjects  int64
	poolTotal    int32
	poolAcquired int32
	poolIdle     int32
	redisMemMB   float64
	redisKeys    int64
	openFDs      int
	usageRows    int64
	auditEvents  int64
	completed    int64
}

// soakConfig holds the run shape. Defaults are the acceptance profile from the
// card; SOAK_DURATION is the required configurability, the rest exist for
// cheap mechanic validation and diagnostics and never change the acceptance
// defaults of the `make soak-gateway` run.
type soakConfig struct {
	duration       time.Duration
	users          int
	pacing         time.Duration
	sampleInterval time.Duration
}

func loadSoakConfig() soakConfig {
	cfg := soakConfig{
		duration:       30 * time.Minute,
		users:          200,
		pacing:         200 * time.Millisecond,
		sampleInterval: 10 * time.Second,
	}
	if v := os.Getenv("SOAK_DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.duration = d
		}
	}
	if v := os.Getenv("SOAK_USERS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.users = n
		}
	}
	if v := os.Getenv("SOAK_PACING"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.pacing = d
		}
	}
	if v := os.Getenv("SOAK_SAMPLE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.sampleInterval = d
		}
	}
	return cfg
}

// soakOpenFDs counts process file descriptors. Linux exposes /proc/self/fd;
// darwin's fdesc mount serves the same listing at /dev/fd. -1 means the
// platform supports neither; the series is then reported but not judged.
func soakOpenFDs() int {
	dir := "/proc/self/fd"
	if _, err := os.Stat(dir); err != nil {
		dir = "/dev/fd"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return -1
	}
	return len(entries)
}

// soakProjectedGrowth fits a least-squares line over (t, y) samples and
// returns the fitted growth slope*(tLast-tFirst): how much the series would
// grow if its average trend continued across the whole run. A sawtooth series
// (heap riding the GC phase at sample time) fits to a near-flat line unless
// real accumulation underlies the oscillation — exactly the "beyond noise"
// property the drift verdict needs. ok=false rejects degenerate input.
func soakProjectedGrowth(ts, ys []float64) (float64, bool) {
	if len(ts) != len(ys) || len(ts) < 3 {
		return 0, false
	}
	n := float64(len(ts))
	var sx, sy, sxy, sxx float64
	for i := range ts {
		sx += ts[i]
		sy += ys[i]
		sxy += ts[i] * ys[i]
		sxx += ts[i] * ts[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, false
	}
	slope := (n*sxy - sx*sy) / den
	return slope * (ts[len(ts)-1] - ts[0]), true
}

// soakMedian returns the median without touching the input order.
func soakMedian(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func TestGatewaySoak(t *testing.T) {
	if testing.Short() {
		t.Skip("soak needs the sustained loop")
	}
	cfg := loadSoakConfig()
	env := newSliceEnv(t, 1<<30)
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	// Stub provider: ~1ms fixed latency, the same floor the perf and
	// concurrency gates use, so the run measures gateway behavior rather than
	// provider variance (provider-side soak is out of the card's scope).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`))
	}))
	t.Cleanup(upstream.Close)

	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('soak-stub', $1, 'env:SOAK_STUB_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Setenv("SOAK_STUB_KEY", "soak-key")
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('soak-fast') ON CONFLICT DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority) VALUES ('soak-fast','soak-stub','soak-fast',100) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('soak-fast','soak-stub',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
		`UPDATE dlp_rules SET action='tokenize', except_roles='{privileged}' WHERE name='internal_phone'`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `UPDATE dlp_rules SET except_roles='{}', action='redact' WHERE name='internal_phone';
			DELETE FROM model_routes WHERE alias='soak-fast'; DELETE FROM model_pricing WHERE alias='soak-fast';
			DELETE FROM model_aliases WHERE alias='soak-fast'; DELETE FROM providers WHERE name='soak-stub';
			DELETE FROM usage_records WHERE alias='soak-fast'; DELETE FROM dlp_tokens;`)
	})

	// Production wiring, identical to the perf and concurrency gates: control
	// caches, table policy, batched vault tokenizer, routed LLM, pricing.
	// Nothing is tuned out of the hot path; governance stays ON for the whole
	// run (TASK-033 fail-safe rules).
	caches := gateway.NewControlCaches(gateway.ControlCacheTTL, gateway.ControlSourcesFromStore(env.store))
	env.srv.Policy = &policy.TablePolicy{LoadRules: caches.PolicyRules()}
	scanner := dlp.NewTableScanner(env.store.LoadDLPRules)
	scanner.Tokenizer = func(c context.Context, ruleName, value string) (string, error) {
		return env.store.TokenizeDLP(c, "secops", ruleName, value)
	}
	scanner.TokenizeBatch = func(c context.Context, ruleName string, values []string) (map[string]string, error) {
		return env.store.BatchTokenizeDLP(c, "secops", ruleName, values)
	}
	env.srv.DLP = scanner
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   caches.ResolvePlan(),
		ResolveTarget: caches.ResolveTarget(),
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{Client: llmkernel.NewProviderHTTPClient()},
	}
	env.srv.Pricing = caches.PricingKnown()
	applierCtx, applierCancel := context.WithCancel(context.Background())
	env.srv.StartTouchApplier(applierCtx)
	t.Cleanup(applierCancel)
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "soak-user@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "soak-user@example.com"))

	payload, _ := json.Marshal(map[string]any{
		"model": "soak-fast",
		"messages": []map[string]string{
			{"role": "user", "content": "给 13812345678 发短信说上线了。"},
		},
	})

	ts := httptest.NewServer(env.handler)
	t.Cleanup(ts.Close)
	httpClient := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2048, MaxIdleConns: 2048}}

	// Redis probes for the sampler: the fixed-window limiter is the only Redis
	// consumer and its keys carry the window TTL, so both memory and key count
	// must stay flat — growth here means an unbounded structure on the hot
	// path (TASK-040 made counter keys expire for exactly this reason).
	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("EAP_TEST_REDIS_ADDR")})
	t.Cleanup(func() { _ = rdb.Close() })

	countRows := func(query string) int64 {
		t.Helper()
		var n int64
		if err := env.pool.QueryRow(ctx, query).Scan(&n); err != nil {
			t.Fatalf("count query %q: %v", query, err)
		}
		return n
	}

	var (
		sampleMu  sync.Mutex
		samples   []soakSample
		latMu     sync.Mutex
		latencies []float64
		completed int64
		non200    int64
		errMu     sync.Mutex
		errNotes  []string
	)

	// Warm-up: pay lazy pool growth and page-cache misses so the drift series
	// start from the real steady state (same rationale as the gate). Warm-up
	// successes still count toward the durable-ledger comparison — usage rows
	// exist for every request, warm-up or not — but their latencies stay out
	// of the tail statistics, which measure the sustained phase. A forced GC
	// afterwards pins the post-GC heap baseline that the final drain is
	// judged against — raw HeapAlloc samples during the run ride the GC
	// sawtooth and cannot by themselves separate a leak from GC phase.
	runWave := func(clients, perClient int) {
		var wg sync.WaitGroup
		for i := 0; i < clients; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < perClient; j++ {
					req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
						ts.URL+"/v1/chat/completions", bytes.NewReader(payload))
					req.Header.Set("Authorization", "Bearer "+keyValue)
					req.Header.Set("X-Agent-Workspace", "secops")
					req.Header.Set("Content-Type", "application/json")
					resp, err := httpClient.Do(req)
					if err != nil {
						atomic.AddInt64(&non200, 1)
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						atomic.AddInt64(&completed, 1)
					} else {
						atomic.AddInt64(&non200, 1)
					}
				}
			}()
		}
		wg.Wait()
	}
	runWave(50, 4)
	runtime.GC()
	runtime.GC()
	time.Sleep(500 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()
	var baselineMem runtime.MemStats
	runtime.ReadMemStats(&baselineMem)
	baselineHeapMB := float64(baselineMem.HeapAlloc) / (1 << 20)

	recordSample := func(at time.Duration) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		st := env.pool.Stat()
		var redisMemMB float64
		if infoMap, err := rdb.InfoMap(ctx, "memory").Result(); err == nil {
			// The reply's section header is "# Memory", so the map key is
			// capitalized even though the requested section is lowercase.
			if raw, ok := infoMap["Memory"]["used_memory"]; ok {
				if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
					redisMemMB = v / (1 << 20)
				}
			}
		}
		var redisKeys int64
		if n, err := rdb.DBSize(ctx).Result(); err == nil {
			redisKeys = n
		}
		s := soakSample{
			at:           at,
			goroutines:   runtime.NumGoroutine(),
			heapAllocMB:  float64(mem.HeapAlloc) / (1 << 20),
			heapObjects:  int64(mem.HeapObjects),
			poolTotal:    st.TotalConns(),
			poolAcquired: st.AcquiredConns(),
			poolIdle:     st.IdleConns(),
			redisMemMB:   redisMemMB,
			redisKeys:    redisKeys,
			openFDs:      soakOpenFDs(),
			completed:    atomic.LoadInt64(&completed),
			// Durable-side probes: usage rows for this alias and the llm.*
			// audit event set. Either stalling while requests complete is a
			// backlog; either outgrowing the request count is phantom data.
			usageRows:   countRows(`SELECT count(*) FROM usage_records WHERE alias = 'soak-fast'`),
			auditEvents: countRows(`SELECT count(*) FROM audit_events WHERE type LIKE 'llm.%'`),
		}
		sampleMu.Lock()
		samples = append(samples, s)
		sampleMu.Unlock()
	}

	doneCh := make(chan struct{})
	started := time.Now()
	go func() {
		ticker := time.NewTicker(cfg.sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case at := <-ticker.C:
				recordSample(at.Sub(started))
			case <-doneCh:
				return
			}
		}
	}()

	// Load phase: cfg.users in-flight workers with cfg.pacing between
	// requests (sleep-then-request, the same order as the gate's paced
	// budget profile). Worker errors are counted, never fatal — one flake
	// must not end a soak whose verdict already requires zero non-200s.
	var wg sync.WaitGroup
	deadline := started.Add(cfg.duration)
	for i := 0; i < cfg.users; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				time.Sleep(cfg.pacing)
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
					ts.URL+"/v1/chat/completions", bytes.NewReader(payload))
				req.Header.Set("Authorization", "Bearer "+keyValue)
				req.Header.Set("X-Agent-Workspace", "secops")
				req.Header.Set("Content-Type", "application/json")
				t0 := time.Now()
				resp, err := httpClient.Do(req)
				if err != nil {
					atomic.AddInt64(&non200, 1)
					errMu.Lock()
					if len(errNotes) < 5 {
						errNotes = append(errNotes, fmt.Sprintf("transport: %v", err))
					}
					errMu.Unlock()
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				latMs := float64(time.Since(t0).Microseconds()) / 1000
				if resp.StatusCode != http.StatusOK {
					atomic.AddInt64(&non200, 1)
					errMu.Lock()
					if len(errNotes) < 5 {
						errNotes = append(errNotes, fmt.Sprintf("status %d", resp.StatusCode))
					}
					errMu.Unlock()
					continue
				}
				atomic.AddInt64(&completed, 1)
				latMu.Lock()
				latencies = append(latencies, latMs)
				latMu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(doneCh)
	elapsed := time.Since(started)
	recordSample(elapsed) // terminal sample pins the end of every series

	sampleMu.Lock()
	frozen := append([]soakSample(nil), samples...)
	sampleMu.Unlock()
	latMu.Lock()
	wholeRun := latencies
	latMu.Unlock()

	report := new(strings.Builder)
	fmt.Fprintf(report, "\n== Gateway soak (full governance ON) ==\n")
	fmt.Fprintf(report, "config: duration=%s users=%d pacing=%s sample_interval=%s race_build=%v\n",
		cfg.duration, cfg.users, cfg.pacing, cfg.sampleInterval, raceEnabled)
	fmt.Fprintf(report, "elapsed=%s completed=%d non200=%d\n",
		elapsed.Round(time.Second), atomic.LoadInt64(&completed), atomic.LoadInt64(&non200))

	// Per-sample table. SOAK_SAMPLE lines are grep-able so docs/verify/soak.md
	// can carry the real table without manual transcription.
	fmt.Fprintf(report, "%-8s %-11s %-9s %-12s %-6s %-10s %-6s %-9s %-10s %-5s %-10s %-10s %-9s\n",
		"t", "goroutines", "heapMB", "heapObjs", "poolT", "poolAcq", "poolI", "redisMB", "redisKeys", "fds", "usage", "audit", "reqs")
	for _, s := range frozen {
		fmt.Fprintf(report, "SOAK_SAMPLE %-7v %-11d %-9.1f %-12d %-6d %-10d %-6d %-9.1f %-10d %-5d %-10d %-10d %-9d\n",
			s.at.Round(time.Second), s.goroutines, s.heapAllocMB, s.heapObjects,
			s.poolTotal, s.poolAcquired, s.poolIdle, s.redisMemMB, s.redisKeys,
			s.openFDs, s.usageRows, s.auditEvents, s.completed)
	}

	var failures []string

	// --- Verdict 1: zero non-200s across the whole run.
	if n := atomic.LoadInt64(&non200); n != 0 {
		failures = append(failures, fmt.Sprintf("non-200 responses: %d (first notes: %v)", n, errNotes))
	}

	// --- Verdict 2: whole-run p95 inside the PRD §9 budget, same non-race
	// semantics as the gate, plus first-vs-last third percentiles — a slow
	// leak that only shows up minutes in must be visible in the split.
	p50 := concurrencyPct(wholeRun, 50)
	p95 := concurrencyPct(wholeRun, 95)
	p99 := concurrencyPct(wholeRun, 99)
	third := len(wholeRun) / 3
	firstP50 := concurrencyPct(wholeRun[:third], 50)
	firstP95 := concurrencyPct(wholeRun[:third], 95)
	lastP50 := concurrencyPct(wholeRun[len(wholeRun)-third:], 50)
	lastP95 := concurrencyPct(wholeRun[len(wholeRun)-third:], 95)
	fmt.Fprintf(report, "latency ms: whole-run n=%d p50=%.2f p95=%.2f p99=%.2f | first-third p50=%.2f p95=%.2f | last-third p50=%.2f p95=%.2f\n",
		len(wholeRun), p50, p95, p99, firstP50, firstP95, lastP50, lastP95)
	if !raceEnabled && p95 >= 100.0 {
		failures = append(failures, fmt.Sprintf("PRD §9 VIOLATION: whole-run p95 %.2fms >= 100ms budget", p95))
	}

	// --- Verdict 3: resource drift. Each series fails when the least-squares
	// growth projected across the run exceeds its absolute floor OR the
	// last-third sample median outgrows the first-third median by more than
	// twice the floor (the median pair catches a mid-run step the line fit
	// could average away). Floors are far above single-sample noise but far
	// below leak rates that matter at 30 minutes.
	//
	// The fit window is the sustained phase only: the first periodic sample
	// still carries warm-up settling (connection pool growth toward the
	// worker count) and the terminal sample is a post-drain value already
	// judged by verdicts 4 and 5 — neither belongs in a load-phase trend.
	type soakSeries struct {
		name  string
		pick  func(soakSample) float64
		floor float64
	}
	series := []soakSeries{
		{"goroutines", func(s soakSample) float64 { return float64(s.goroutines) }, 50},
		{"heapMB", func(s soakSample) float64 { return s.heapAllocMB }, 128},
		{"heapObjs", func(s soakSample) float64 { return float64(s.heapObjects) }, 3e6},
		// poolAcquired is bounded by the pool cap (32) by construction and is
		// sampled instantaneously, so it oscillates across the cap with the
		// request-wave phase; a tiny absolute floor reads that quantization
		// as trend. A real pool leak manifests as saturation (pegged at the
		// cap) or a dirty drain, both already hard verdicts — so the floor
		// here is half the cap, catching sustained pressure growth.
		{"poolAcquired", func(s soakSample) float64 { return float64(s.poolAcquired) }, 16},
		{"redisMemMB", func(s soakSample) float64 { return s.redisMemMB }, 64},
		{"redisKeys", func(s soakSample) float64 { return float64(s.redisKeys) }, 500},
	}
	// FD count is platform-dependent: only judged when the probe works.
	if fdProbeWorks := soakOpenFDs() >= 0; fdProbeWorks {
		series = append(series, soakSeries{"openFDs", func(s soakSample) float64 { return float64(s.openFDs) }, 100})
	}
	driftSpan := frozen
	if len(frozen) >= 5 {
		driftSpan = frozen[1 : len(frozen)-1]
	}
	for _, ser := range series {
		ts := make([]float64, 0, len(driftSpan))
		ys := make([]float64, 0, len(driftSpan))
		for _, s := range driftSpan {
			ts = append(ts, s.at.Seconds())
			ys = append(ys, ser.pick(s))
		}
		growth, ok := soakProjectedGrowth(ts, ys)
		st := len(ys) / 3
		firstMed, lastMed := soakMedian(ys[:st]), soakMedian(ys[len(ys)-st:])
		fmt.Fprintf(report, "drift %-13s projected=%+9.1f floor=%-7.0f firstThirdMedian=%-10.1f lastThirdMedian=%-10.1f",
			ser.name, growth, ser.floor, firstMed, lastMed)
		switch {
		case !ok:
			fmt.Fprintf(report, " -> SKIPPED (too few samples)\n")
		case growth > ser.floor:
			fmt.Fprintf(report, " -> DRIFT (line fit)\n")
			failures = append(failures, fmt.Sprintf("resource drift [%s]: fitted growth %.1f over the run exceeds floor %.0f", ser.name, growth, ser.floor))
		case lastMed-firstMed > 2*ser.floor:
			fmt.Fprintf(report, " -> DRIFT (median step)\n")
			failures = append(failures, fmt.Sprintf("resource drift [%s]: last-third median %.1f exceeds first-third median %.1f by more than 2x floor %.0f", ser.name, lastMed, firstMed, ser.floor))
		default:
			fmt.Fprintf(report, " -> flat\n")
		}
	}

	// --- Verdict 4: clean post-run drain. Release the harness client's own
	// idle-connection readers first (they are not the gateway's), let the
	// bounded touch applier flush, then sample twice: a stable count is a
	// drained pool; a falling count was a draining backlog; a growing count
	// is a leak. Mirrors the gate's goroutine hygiene with tighter bounds.
	httpClient.CloseIdleConnections()
	time.Sleep(3 * time.Second)
	drain1 := runtime.NumGoroutine()
	time.Sleep(2 * time.Second)
	drain2 := runtime.NumGoroutine()
	finalPool := env.pool.Stat()
	fmt.Fprintf(report, "drain: baseline=%d drain1=%d drain2=%d pool(total=%d acquired=%d idle=%d)\n",
		baselineGoroutines, drain1, drain2, finalPool.TotalConns(), finalPool.AcquiredConns(), finalPool.IdleConns())
	if drain2 > drain1+5 {
		failures = append(failures, fmt.Sprintf("drain: goroutines still growing after load (drain1=%d drain2=%d)", drain1, drain2))
	}
	if drain2 > baselineGoroutines+50 {
		failures = append(failures, fmt.Sprintf("drain: goroutines did not return near baseline (baseline=%d settled=%d)", baselineGoroutines, drain2))
	}
	if finalPool.AcquiredConns() > 1 {
		failures = append(failures, fmt.Sprintf("drain: pool connections still checked out (%d acquired)", finalPool.AcquiredConns()))
	}

	// --- Verdict 5: no audit/usage backlog, no phantom data. The terminal
	// sample runs after every worker returned, so durable rows must equal
	// completed requests exactly (writes are synchronous in the governance
	// batch). Every routed decision carries exactly one llm.* audit event in
	// this run (policy.decision uses its own type), so the audit count has a
	// hard floor of one per completed request — below that, delivery was
	// lost; the sampled series shows any async pile-up.
	terminal := frozen[len(frozen)-1]
	usageDelta := terminal.usageRows - terminal.completed
	auditFloor := terminal.completed
	fmt.Fprintf(report, "backlog: usage_rows=%d completed=%d delta=%+d audit_events=%d audit_floor=%d\n",
		terminal.usageRows, terminal.completed, usageDelta, terminal.auditEvents, auditFloor)
	if usageDelta != 0 {
		failures = append(failures, fmt.Sprintf("audit/usage ledger mismatch: usage_rows=%d completed=%d (delta %+d) — loss or backlog", terminal.usageRows, terminal.completed, usageDelta))
	}
	if terminal.auditEvents < auditFloor {
		failures = append(failures, fmt.Sprintf("audit backlog: llm.* events=%d below guaranteed floor %d", terminal.auditEvents, auditFloor))
	}

	// --- Verdict 6: post-GC heap returns near the warm-up baseline. Raw
	// samples ride the GC sawtooth; a forced-GC comparison is the honest
	// memory verdict. Bounded derived caches (control data, vault memo) may
	// legitimately hold more after traffic than before — the bound allows
	// 1.5x baseline + 16MiB of that before calling it drift.
	runtime.GC()
	runtime.GC()
	time.Sleep(300 * time.Millisecond)
	var finalMem runtime.MemStats
	runtime.ReadMemStats(&finalMem)
	finalHeapMB := float64(finalMem.HeapAlloc) / (1 << 20)
	heapCeiling := baselineHeapMB*1.5 + 16
	fmt.Fprintf(report, "post-GC heap: baseline=%.1fMB final=%.1fMB ceiling=%.1fMB heapObjs baseline=%d final=%d\n",
		baselineHeapMB, finalHeapMB, heapCeiling, baselineMem.HeapObjects, finalMem.HeapObjects)
	if finalHeapMB > heapCeiling {
		failures = append(failures, fmt.Sprintf("post-GC heap grew beyond the derived-cache bound: baseline %.1fMB -> final %.1fMB (ceiling %.1fMB)", baselineHeapMB, finalHeapMB, heapCeiling))
	}

	// --- Verdict assembly: every criterion is reported, then enforced.
	verdict := "PASS"
	if len(failures) > 0 {
		verdict = "FAIL"
	}
	fmt.Fprintf(report, "SOAK_VERDICT overall=%s failures=%d\n", verdict, len(failures))
	for i, f := range failures {
		fmt.Fprintf(report, "SOAK_FAILURE %d: %s\n", i+1, f)
	}
	t.Log(report.String())
	for _, f := range failures {
		t.Errorf("soak stability: %s", f)
	}
}
