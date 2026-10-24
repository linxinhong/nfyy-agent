//go:build integration

// M0-M3 integration journey (TASK-006 acceptance): one public endpoint —
// login, key creation, an approved LLM protocol call and an allowed MCP read
// tool; revocation, cross-workspace, DLP and policy failures fail closed with
// trace/audit evidence.
package main_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/apikey"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
)

func newJourneyKernel(t *testing.T, providerURL string) *llmkernel.Kernel {
	t.Helper()
	k, err := llmkernel.New(context.Background(), llmkernel.Config{
		ProviderBaseURL: providerURL,
		ProviderKey:     "sk-provider-secret",
		Model:           "stub-model",
	})
	if err != nil {
		t.Fatalf("journey kernel: %v", err)
	}
	return k
}

func journeyPolicy() llmkernel.PolicyEngine {
	return &policy.Allowlist{Workspaces: []string{"secops"}, Models: []string{"stub-model"}}
}

func journeyDLP() llmkernel.DLPEngine { return dlp.Scanner{} }

type journeyUsageRecorder struct {
	records int
}

func (j *journeyUsageRecorder) Record(_ context.Context, _ llmkernel.UsageRecord) { j.records++ }

func TestM0M3JourneyIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()

	// Provider stub for the LLM kernel (the "provider" side of the boundary).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-stub","object":"chat.completion","model":"stub-model","choices":[{"index":0,"message":{"role":"assistant","content":"journey ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	t.Cleanup(upstream.Close)

	usage := &journeyUsageRecorder{}
	env.srv.LLM = newJourneyKernel(t, upstream.URL)
	env.srv.Policy = journeyPolicy()
	env.srv.DLP = journeyDLP()
	env.srv.Usage = usage
	// Routes snapshot at Handler() build time — rebuild with the LLM route.
	env.handler = env.srv.Handler()

	// MCP git tool rides the env credential and a stub upstream.
	t.Setenv("EAP_MCP_GIT_TOKEN", "git-secret-token")
	gitUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer git-secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"content":"readme"}`))
	}))
	t.Cleanup(gitUpstream.Close)
	if _, err := env.pool.Exec(ctx,
		`UPDATE mcp_tools SET endpoint_url = $1 WHERE name = 'git_read_file'`,
		gitUpstream.URL+"/repos/{repo}/contents/{path}"); err != nil {
		t.Fatalf("repoint tool: %v", err)
	}

	// 1. Login (syncs user + membership) and mint an employee key.
	sessionToken := mcpLogin(t, env, "journey@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "journey@example.com"))
	authz := map[string]string{"Authorization": "Bearer " + keyValue, "X-Agent-Workspace": "secops"}

	// 2. Approved LLM protocol through the public endpoint.
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"stub-model","messages":[{"role":"user","content":"hello journey"}]}`, authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("llm journey: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "journey ok") {
		t.Fatalf("llm journey payload wrong: %s", rec.Body.String())
	}
	if usage.records != 1 {
		t.Fatalf("usage hook must fire once, got %d", usage.records)
	}

	// 3. Allowed MCP read tool through the same endpoint.
	rec = env.do(t, http.MethodPost, "/mcp/gitlab", keyValue,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"acme/app","path":"README.md"}}}`, authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("mcp journey: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "readme") || strings.Contains(rec.Body.String(), "git-secret-token") {
		t.Fatalf("mcp journey payload wrong: %s", rec.Body.String())
	}

	// 4. DLP failure fails closed with the frozen code.
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"stub-model","messages":[{"role":"user","content":"my key is sk-abcdefghij0123456789AB"}]}`, authz)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "dlp_blocked") {
		t.Fatalf("dlp journey: %d %s", rec.Code, rec.Body.String())
	}

	// 5. Policy failure (model off the allowlist) fails closed.
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"unlisted-model","messages":[{"role":"user","content":"hi"}]}`, authz)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "policy_denied") {
		t.Fatalf("policy journey: %d %s", rec.Code, rec.Body.String())
	}

	// 6. Cross-workspace access fails closed on both surfaces.
	badWS := map[string]string{"Authorization": "Bearer " + keyValue, "X-Agent-Workspace": "finance"}
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"stub-model","messages":[{"role":"user","content":"hi"}]}`, badWS)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "workspace_forbidden") {
		t.Fatalf("llm cross-workspace: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/mcp/gitlab", keyValue,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"a","path":"b"}}}`, badWS)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "workspace_forbidden") {
		t.Fatalf("mcp cross-workspace: %d %s", rec.Code, rec.Body.String())
	}

	// 7. Revocation (control-plane session) closes every door immediately.
	keyID := seededKeyID(t, env, keyValue)
	if rec := env.do(t, http.MethodDelete, "/enterprise/v1/keys/"+keyID, sessionToken, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	checks := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/chat/completions", `{"model":"stub-model","messages":[{"role":"user","content":"hi"}]}`},
		{http.MethodPost, "/mcp/gitlab", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"git_read_file","arguments":{"repo":"a","path":"b"}}}`},
		{http.MethodGet, "/enterprise/v1/me", ""},
	}
	for _, c := range checks {
		rec = env.do(t, c.method, c.path, keyValue, c.body, authz)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("revoked key on %s: %d, want 401", c.path, rec.Code)
		}
	}

	// 8. Trace/audit evidence across the journey.
	for _, tc := range []struct {
		query string
		min   int
	}{
		{"SELECT count(*) FROM audit_events WHERE type='gateway.auth.failed' AND outcome='denied'", 1},
		{"SELECT count(*) FROM audit_events WHERE type='mcp.tool.executed' AND outcome='allowed'", 1},
		// Cross-workspace is denied at workspace selection (before a session
		// exists), recorded as gateway.auth.failed; the mcp denied event fires
		// for member callers calling foreign tools (covered in TASK-004 tests).
		{"SELECT count(*) FROM audit_events WHERE type='identity.key.created'", 1},
		{"SELECT count(*) FROM audit_events WHERE type='identity.key.revoked'", 1},
		{"SELECT count(*) FROM audit_events WHERE trace_id <> '' AND type <> 'identity.key.created'", 1},
	} {
		var n int
		if err := env.pool.QueryRow(ctx, tc.query).Scan(&n); err != nil {
			t.Fatalf("audit %q: %v", tc.query, err)
		}
		if n < tc.min {
			t.Errorf("audit evidence missing for %q (got %d)", tc.query, n)
		}
	}
}

func seededKeyID(t *testing.T, env *sliceEnv, keyValue string) string {
	t.Helper()
	var id string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT key_id FROM api_keys WHERE key_hash = $1`, apikey.Hash(keyValue)).Scan(&id); err != nil {
		t.Fatalf("load seeded key id: %v", err)
	}
	return id
}
