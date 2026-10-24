//go:build integration

// TASK-035 / ADR-0024 acceptance: a workspace-bound agk_* key authorizes
// header-less requests inside its bound workspace; the frozen selection
// precedence is header → key binding → fail closed. The binding only
// nominates: membership is re-resolved on every request, so removing the
// owner from the workspace invalidates a bound key immediately, and a header
// override stays membership-gated.
//
// Run via `make verify-key-bound-workspace`.
package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/apikey"
	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/policy"
)

// assertAuditDetail asserts at least one audit event of the given type whose
// JSON detail carries detail[key] == value.
func assertAuditDetail(t *testing.T, pool *pgxpool.Pool, typ, key, value string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE type = $1 AND detail->>$2 = $3`, typ, key, value).Scan(&n); err != nil {
		t.Fatalf("audit detail query: %v", err)
	}
	if n == 0 {
		t.Errorf("audit evidence missing for %s with %s=%s", typ, key, value)
	}
}

// bindPolicy allows the model for the workspaces under test.
func bindPolicy() *policy.Allowlist {
	return &policy.Allowlist{Workspaces: []string{"finance", "secops"}, Models: []string{"stub-model"}}
}

func newBindingEnv(t *testing.T) *sliceEnv {
	t.Helper()
	env := newSliceEnv(t, 1000)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-stub","object":"chat.completion","model":"stub-model","choices":[{"index":0,"message":{"role":"assistant","content":"binding ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	t.Cleanup(upstream.Close)

	env.srv.LLM = newJourneyKernel(t, upstream.URL)
	env.srv.Policy = bindPolicy()
	env.srv.DLP = dlp.Scanner{}
	env.handler = env.srv.Handler()
	return env
}

func seedBoundKey(t *testing.T, env *sliceEnv, userID, workspace string) string {
	t.Helper()
	g, err := apikey.Generate(apikey.EnvLive)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	ev := audit.NewEvent(audit.EventAuthKeyCreated, audit.OutcomeAllowed)
	if _, err := env.store.CreateAPIKey(context.Background(), userID, apikey.Hash(g.Full), g.Prefix, "bound", workspace, nil, ev); err != nil {
		t.Fatalf("plant bound key: %v", err)
	}
	return g.Full
}

func chatBody(t *testing.T, env *sliceEnv, key string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return env.do(t, http.MethodPost, "/v1/chat/completions", key,
		`{"model":"stub-model","messages":[{"role":"user","content":"hello binding"}]}`, headers)
}

func TestAPIKeyWorkspaceBindingIntegration(t *testing.T) {
	env := newBindingEnv(t)
	ctx := context.Background()

	// A finance member gets a finance-bound key. Header-less chat must
	// resolve to finance through the binding alone.
	mcpLogin(t, env, "bound@example.com", "finance")
	userID := mcpUserID(t, env, "bound@example.com")
	boundKey := seedBoundKey(t, env, userID, "finance")

	rec := chatBody(t, env, boundKey, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "binding ok") {
		t.Fatalf("bound key header-less chat: %d %s", rec.Code, rec.Body.String())
	}

	// The binding is only a default: an explicit header for a workspace the
	// user does not belong to is still denied by membership.
	rec = chatBody(t, env, boundKey, map[string]string{identity.HeaderWorkspace: "secops"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "workspace_forbidden") {
		t.Fatalf("bound key cross-workspace header: %d %s", rec.Code, rec.Body.String())
	}

	// An unbound key without the header still fails closed — unchanged
	// behavior is the regression guard for this slice.
	unboundKey := seedKey(t, env, userID)
	rec = chatBody(t, env, unboundKey, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "workspace_forbidden") {
		t.Fatalf("unbound key header-less chat: %d %s", rec.Code, rec.Body.String())
	}

	// Header wins over the binding when the user is a member of both.
	mcpLogin(t, env, "dual@example.com", "finance")
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id) VALUES ('secops', $1)
		 ON CONFLICT DO NOTHING`, mcpUserID(t, env, "dual@example.com")); err != nil {
		t.Fatalf("add second membership: %v", err)
	}
	dualKey := seedBoundKey(t, env, mcpUserID(t, env, "dual@example.com"), "finance")
	rec = chatBody(t, env, dualKey, map[string]string{identity.HeaderWorkspace: "secops"})
	if rec.Code != http.StatusOK {
		t.Fatalf("member header override: %d %s", rec.Code, rec.Body.String())
	}

	// Token exchange through the binding alone mints a finance-scoped token.
	var ex struct {
		AccessToken string `json:"access_token"`
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/token/exchange", boundKey,
		`{"audience":"llm-gateway"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("bound key exchange: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ex); err != nil {
		t.Fatalf("exchange payload: %v", err)
	}
	claims, err := env.verifier(t).Verify(ex.AccessToken, "llm-gateway")
	if err != nil {
		t.Fatalf("exchanged token must verify: %v", err)
	}
	if claims.WorkspaceID != "finance" {
		t.Fatalf("token workspace = %q, want finance", claims.WorkspaceID)
	}

	// The binding never outlives membership: removing the owner from the
	// workspace denies the next header-less call on the inference surface,
	// and the audited denial (token exchange) names key_binding as the
	// selection origin.
	if _, err := env.pool.Exec(ctx,
		`DELETE FROM workspace_members WHERE user_id = $1 AND workspace_id = 'finance'`, userID); err != nil {
		t.Fatalf("remove membership: %v", err)
	}
	rec = chatBody(t, env, boundKey, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "workspace_forbidden") {
		t.Fatalf("bound key after membership removal: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/token/exchange", boundKey,
		`{"audience":"llm-gateway"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bound key exchange after removal: %d %s", rec.Code, rec.Body.String())
	}
	assertAuditDetail(t, env.pool, "gateway.auth.failed", "workspace_origin", "key_binding")
}

// TestAPIKeyBindingCreationGate covers the control-plane half: only a
// workspace the creator belongs to can be bound, and the binding is visible
// in the creation response and key listing.
func TestAPIKeyBindingCreationGate(t *testing.T) {
	env := newSliceEnv(t, 1000)
	// Binding to a workspace the creator does not belong to fails closed.
	session := mcpLogin(t, env, "creator@example.com", "finance")
	rec := env.do(t, http.MethodPost, "/enterprise/v1/keys", session,
		`{"name":"ci","workspace":"secops"}`, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "workspace_forbidden") {
		t.Fatalf("non-member binding: %d %s", rec.Code, rec.Body.String())
	}

	// A member workspace binds and is echoed in the response...
	rec = env.do(t, http.MethodPost, "/enterprise/v1/keys", session,
		`{"name":"ci","workspace":"finance"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("member binding: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		KeyID     string `json:"key_id"`
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	if created.Workspace != "finance" {
		t.Fatalf("created workspace = %q, want finance", created.Workspace)
	}

	// ...and in the owner's key listing.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/keys", session, "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"workspace":"finance"`) {
		t.Fatalf("key listing must expose binding: %d %s", rec.Code, rec.Body.String())
	}
	assertAuditTypes(t, env.pool, "identity.key.created")
}
