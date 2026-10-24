//go:build integration

// TASK-043 acceptance: the steady-state request path issues a bounded number
// of database round trips, proven by a pgx Tracer counting seam wired into
// the real pool:
//   - the governance commit (audit set + usage row) travels as exactly ONE
//     SendBatch of four statements (BEGIN, audit insert, usage insert, COMMIT);
//   - the auth chain resolves key + owner + both candidate memberships in one
//     fused SELECT, and the old per-request IsMember probe is never issued;
//   - repeated vault tokenize hits are served from the bounded process-local
//     memo with ZERO additional queries, byte-identical pseudonyms.
package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	gateway "enterprise-agent-platform/apps/gateway"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/mcpgateway"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/store"
	"enterprise-agent-platform/packages/token"
)

// rttTracer counts round trips at the pgx seam: every standalone Query/Exec
// is one entry; every SendBatch is one group (one flush, one round trip)
// holding its statements verbatim.
type rttTracer struct {
	mu      sync.Mutex
	queries []string
	batches [][]string
}

func (t *rttTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries = append(t.queries, data.SQL)
	t.mu.Unlock()
	return ctx
}
func (t *rttTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (t *rttTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	t.mu.Lock()
	stmts := make([]string, 0, data.Batch.Len())
	for _, qq := range data.Batch.QueuedQueries {
		stmts = append(stmts, qq.SQL)
	}
	t.batches = append(t.batches, stmts)
	t.mu.Unlock()
	return ctx
}
func (t *rttTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (t *rttTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}

func (t *rttTracer) snapshot() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.queries), len(t.batches)
}

// rttAllowAllDLP passes everything (the vault memo test drives BatchTokenizeDLP
// directly; the chat path only needs a configured engine).
type rttAllowAllDLP struct{}

func (rttAllowAllDLP) Scan(_ context.Context, text string) (llmkernel.DLPResult, error) {
	return llmkernel.DLPResult{Redacted: text}, nil
}

func TestGovernanceRTTCountingIntegration(t *testing.T) {
	tracer := &rttTracer{}
	ctx := context.Background()

	adminCfg, err := pgxpool.ParseConfig(os.Getenv("EAP_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse admin url: %v", err)
	}
	adminCfg.ConnConfig.Tracer = tracer
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()

	testDB := fmt.Sprintf("eap_rtt_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, testDB)); err != nil {
		t.Fatalf("drop db: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, testDB)); err != nil {
		t.Fatalf("create db: %v", err)
	}

	testURL := strings.Replace(os.Getenv("EAP_TEST_DATABASE_URL"), "/postgres?", "/"+testDB+"?", 1)
	poolCfg, err := pgxpool.ParseConfig(testURL)
	if err != nil {
		t.Fatalf("parse test url: %v", err)
	}
	poolCfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	migrationsDir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if err := store.Migrate(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st := store.New(pool)

	signingKey, err := st.EnsureSigningKey(ctx, store.GenerateSigningKeyDER)
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	verifier, err := token.NewEd25519Verifier(token.VerificationKey{KeyID: signingKey.Kid, Public: signingKey.PublicKey})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	signer, err := token.NewEd25519Signer(signingKey.Kid, "https://agent.company.com/", signingKey.PrivateKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	jwksBody, _ := json.Marshal(mustJWKS(t, token.VerificationKey{KeyID: signingKey.Kid, Public: signingKey.PublicKey}))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"rtt ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	if _, err := pool.Exec(ctx, `
		INSERT INTO providers (name, base_url, credential_ref) VALUES ('rtt-gate', $1, 'env:RTT_GATE_KEY')
		ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Setenv("RTT_GATE_KEY", "rtt-gate-key")
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('stub-model') ON CONFLICT (alias) DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority, weight) VALUES ('stub-model','rtt-gate','stub-model',100,1) ON CONFLICT DO NOTHING`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed llm control data: %v", err)
		}
	}

	srv := &gateway.Server{
		PG:        gateway.PgPinger{Pool: pool},
		Redis:     stubPinger{},
		Store:     st,
		Signer:    signer,
		Verifier:  verifier,
		JWKSBody:  jwksBody,
		AuthChain: st.GetAPIKeyAuthByHash,
		Rate:      &gateway.LocalWindow{Limit: 1000, Window: time.Minute},
		DevMode:   true,
		LLMRouter: &llmkernel.Router{
			ResolvePlan:   st.RoutePlanFor,
			ResolveTarget: st.ResolveProviderTarget,
			Credential:    srvCredentialResolver(),
			Client:        &llmkernel.OpenAICompatClient{},
		},
		Policy: &policy.Allowlist{Workspaces: []string{"finance"}, Models: []string{"stub-model"}},
		DLP:    rttAllowAllDLP{},
	}
	env := &sliceEnv{handler: srv.Handler(), srv: srv, store: st, pool: pool}

	mcpLogin(t, env, "rtt@example.com", "finance")
	keyValue := seedKey(t, env, mcpUserID(t, env, "rtt@example.com"))
	authz := map[string]string{"Authorization": "Bearer " + keyValue, identity.HeaderWorkspace: "finance"}

	qBefore, bBefore := tracer.snapshot()
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"stub-model","messages":[{"role":"user","content":"hello rtt"}]}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "rtt ok") {
		t.Fatalf("routed chat: %d %s", rec.Code, rec.Body.String())
	}
	qAfter, bAfter := tracer.snapshot()

	// The governance commit is ONE SendBatch of four statements.
	if bAfter-bBefore != 1 {
		t.Fatalf("expected exactly 1 governance batch, got %d", bAfter-bBefore)
	}
	batch := tracer.batches[bAfter-1]
	if len(batch) != 4 ||
		!strings.HasPrefix(batch[0], "BEGIN") ||
		!strings.Contains(batch[1], "INSERT INTO audit_events") ||
		!strings.Contains(batch[2], "INSERT INTO usage_records") ||
		!strings.HasPrefix(batch[3], "COMMIT") {
		t.Fatalf("governance batch shape wrong: %q", batch)
	}

	// Membership came from the fused auth query: the standalone IsMember
	// probe must not appear anywhere on the request path.
	for _, sql := range tracer.queries[qBefore:qAfter] {
		if strings.Contains(sql, "SELECT EXISTS(") {
			for i, q := range tracer.queries[qBefore:qAfter] {
				t.Logf("query[%d]: %q", i, q)
			}
			t.Fatalf("standalone IsMember probe issued on the fused path")
		}
	}
	fused := false
	for _, sql := range tracer.queries[qBefore:qAfter] {
		if strings.Contains(sql, "LEFT JOIN workspace_members hm") {
			fused = true
		}
	}
	if !fused {
		t.Fatalf("fused auth+membership query not traced among %d queries", qAfter-qBefore)
	}

	// The vault memo serves repeated tokenize hits with zero queries and
	// byte-identical pseudonyms.
	tok1, err := st.BatchTokenizeDLP(ctx, "finance", "internal_phone", []string{"13812345678"})
	if err != nil {
		t.Fatalf("tokenize 1: %v", err)
	}
	q0, _ := tracer.snapshot()
	tok2, err := st.BatchTokenizeDLP(ctx, "finance", "internal_phone", []string{"13812345678"})
	if err != nil {
		t.Fatalf("tokenize 2: %v", err)
	}
	q1, _ := tracer.snapshot()
	if q1 != q0 {
		t.Fatalf("memoized tokenize issued %d extra queries, want 0", q1-q0)
	}
	if tok1["13812345678"] != tok2["13812345678"] {
		t.Fatalf("memo diverged from vault: %q vs %q", tok1["13812345678"], tok2["13812345678"])
	}
}

func srvCredentialResolver() func(ctx context.Context, ref string) (string, error) {
	return func(ctx context.Context, ref string) (string, error) {
		return mcpgateway.EnvResolver{}.Resolve(ctx, ref)
	}
}
