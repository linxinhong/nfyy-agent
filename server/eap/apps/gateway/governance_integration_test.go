//go:build integration

// Provider governance integration (TASK-008 acceptance): one logical model
// routes across two independently configured provider stubs with
// deterministic failover; usage/cost rows are durable and idempotent;
// credential failure and missing pricing fail safely without exposing
// secrets or fabricating zero-cost success.
package main_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gateway "enterprise-agent-platform/apps/gateway"
	"enterprise-agent-platform/packages/llmkernel"
)

func TestProviderGovernanceIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()

	// Two independently configured provider stubs with distinct credentials.
	primaryHits, backupHits := 0, 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key-primary" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		primaryHits++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"from primary"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key-backup" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backupHits++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"from backup"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
	}))
	defer backup.Close()

	// Versioned control data: alias -> two routes; pricing only on backup.
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO providers (name, base_url, credential_ref) VALUES
			('gov-primary', $1, 'env:GOV_PRIMARY_KEY'),
			('gov-backup',  $2, 'env:GOV_BACKUP_KEY')
		ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`,
		primary.URL, backup.URL); err != nil {
		t.Fatalf("seed providers: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO model_aliases (alias) VALUES ('gov-fast') ON CONFLICT DO NOTHING;
		INSERT INTO model_routes (alias, provider, model, priority) VALUES
			('gov-fast', 'gov-primary', 'fast-primary', 100),
			('gov-fast', 'gov-backup',  'fast-backup',  200);
		INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES
			('gov-fast', 'gov-backup', 3.0, 12.0) ON CONFLICT DO NOTHING;`); err != nil {
		t.Fatalf("seed routes: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM model_routes WHERE alias='gov-fast'; DELETE FROM model_pricing WHERE alias='gov-fast'; DELETE FROM model_aliases WHERE alias='gov-fast'; DELETE FROM providers WHERE name LIKE 'gov-%'; DELETE FROM usage_records WHERE alias='gov-fast';`)
	})

	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.srv.Policy = allowAllPolicy{}
	env.srv.DLP = allowAllDLP{}
	env.handler = env.srv.Handler()
	t.Setenv("GOV_PRIMARY_KEY", "key-primary")
	t.Setenv("GOV_BACKUP_KEY", "key-backup")

	// Login + key.
	mcpLogin(t, env, "gov@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "gov@example.com"))
	authz := map[string]string{"Authorization": "Bearer " + keyValue, "X-Agent-Workspace": "secops"}

	// 1. Primary credential ref broken -> deterministic failover to backup.
	if _, err := env.pool.Exec(ctx, `UPDATE providers SET credential_ref = 'env:GOV_MISSING' WHERE name = 'gov-primary'`); err != nil {
		t.Fatalf("break primary credential: %v", err)
	}
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"gov-fast","messages":[{"role":"user","content":"hello governance"}]}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from backup") {
		t.Fatalf("failover journey: %d %s", rec.Code, rec.Body.String())
	}
	if backupHits != 1 || primaryHits != 0 {
		t.Fatalf("expected only backup to serve, primary=%d backup=%d", primaryHits, backupHits)
	}

	// Ledger: one durable row linked to user/workspace/trace, priced by the
	// backup sheet (3.0/MT * 100 in + 12.0/MT * 50 out = 900 micros).
	var (
		provider    string
		costMicros  int64
		costKnown   bool
		traceID     string
		workspaceID string
		userID      string
	)
	if err := env.pool.QueryRow(ctx, `
		SELECT provider, cost_micros, cost_known, trace_id, workspace_id, user_id
		FROM usage_records WHERE alias = 'gov-fast'`).Scan(
		&provider, &costMicros, &costKnown, &traceID, &workspaceID, &userID); err != nil {
		t.Fatalf("usage row: %v", err)
	}
	if provider != "gov-backup" || !costKnown || costMicros != 900 || traceID == "" || workspaceID != "secops" || userID == "" {
		t.Fatalf("ledger row wrong: provider=%s micros=%d known=%t trace=%q ws=%s user=%q", provider, costMicros, costKnown, traceID, workspaceID, userID)
	}

	// 2. Idempotency: replaying the SAME (trace, request, attempt) key with a
	// would-be double charge is a no-op.
	var requestID string
	var attempt int
	if err := env.pool.QueryRow(ctx, `
		SELECT request_id, attempt FROM usage_records WHERE trace_id = $1`, traceID).
		Scan(&requestID, &attempt); err != nil {
		t.Fatalf("load ledger key: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO usage_records
			(trace_id, request_id, attempt, user_id, workspace_id, alias, provider, provider_model,
			 prompt_tokens, completion_tokens, total_tokens, currency, cost_micros, pricing_version, streaming, cost_known)
		VALUES ($1, $2, $3, 'u', 'secops', 'gov-fast', 'gov-backup', 'm', 999, 999, 1998, 'USD', 999999, 1, false, true)
		ON CONFLICT (trace_id, request_id, attempt) DO NOTHING`,
		traceID, requestID, attempt); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	var rows int
	var costAfter int64
	if err := env.pool.QueryRow(ctx, `SELECT count(*), max(cost_micros) FROM usage_records WHERE trace_id = $1`, traceID).Scan(&rows, &costAfter); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 || costAfter != 900 {
		t.Fatalf("idempotency broken: %d rows, cost=%d (replay must be a no-op)", rows, costAfter)
	}

	// 3. All credentials unavailable -> fails safely, no egress.
	if _, err := env.pool.Exec(ctx, `UPDATE providers SET credential_ref = 'env:GOV_MISSING_BOTH'`); err != nil {
		t.Fatalf("break all credentials: %v", err)
	}
	before := backupHits
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"gov-fast","messages":[{"role":"user","content":"x"}]}`, authz)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "credential") {
		t.Fatalf("all-credentials-failed must fail safely: %d %s", rec.Code, rec.Body.String())
	}
	if backupHits != before {
		t.Fatalf("credential failure must not reach the provider")
	}

	// 4. Missing pricing on the serving provider: request succeeds, but the
	// ledger records cost_known=false — never fabricated zero-cost success.
	if _, err := env.pool.Exec(ctx, `
		UPDATE providers SET credential_ref = 'env:GOV_PRIMARY_KEY' WHERE name = 'gov-primary';
		DELETE FROM model_pricing WHERE alias = 'gov-fast' AND provider = 'gov-backup';`); err != nil {
		t.Fatalf("remove pricing: %v", err)
	}
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"gov-fast","messages":[{"role":"user","content":"y"}]}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from primary") {
		t.Fatalf("primary path: %d %s", rec.Code, rec.Body.String())
	}
	var unknownCost int
	if err := env.pool.QueryRow(ctx, `
		SELECT count(*) FROM usage_records
		WHERE alias = 'gov-fast' AND provider = 'gov-primary' AND cost_known = false`).Scan(&unknownCost); err != nil {
		t.Fatalf("unknown-cost query: %v", err)
	}
	if unknownCost != 1 {
		t.Fatalf("unknown pricing must be explicit (cost_known=false), got %d", unknownCost)
	}

	// 5. Unknown logical model: no enabled route -> 404, no egress.
	before = primaryHits
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"no-such-alias","messages":[{"role":"user","content":"z"}]}`, authz)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown alias: %d %s", rec.Code, rec.Body.String())
	}
	if primaryHits != before {
		t.Fatalf("unknown alias must not egress")
	}

	// 6. Credentials never appear in any response body.
	if strings.Contains(rec.Body.String(), "key-primary") || strings.Contains(rec.Body.String(), "key-backup") {
		t.Fatalf("credential leaked in response: %s", rec.Body.String())
	}
}

// allowAllPolicy / allowAllDLP isolate the governance journey from the
// baseline engines (those have their own coverage in the M0-M3 gate).
type allowAllPolicy struct{}

func (allowAllPolicy) Decide(_ context.Context, in llmkernel.PolicyInput) (llmkernel.Decision, error) {
	return llmkernel.Decision{Allowed: true}, nil
}

type allowAllDLP struct{}

func (allowAllDLP) Scan(_ context.Context, text string) (llmkernel.DLPResult, error) {
	return llmkernel.DLPResult{Redacted: text}, nil
}

var _ = gateway.PgPinger{}
