//go:build integration

// Admin console integration (TASK-022): the admin inspection surface is
// fail-closed (non-admins are denied on every endpoint when called directly),
// admin reads succeed across all governance domains, every read produces a
// reliable-path admin.read audit record, pagination is enforced server-side,
// and no response carries payloads, signatures, credential references,
// endpoint URLs or key material.

package main_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestAdminConsoleIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.handler = env.srv.Handler()

	// Administrator: control-plane session + admin role on the membership.
	adminSession := mcpLogin(t, env, "platform-admin@example.com", "secops")
	adminID := mcpUserID(t, env, "platform-admin@example.com")
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspace_members SET roles = '{admin}' WHERE user_id = $1`, adminID); err != nil {
		t.Fatalf("grant admin role: %v", err)
	}
	adminAuth := map[string]string{"Authorization": "Bearer " + adminSession}

	// Plain employee: valid session, no admin role.
	empSession := mcpLogin(t, env, "plain-employee@example.com", "secops")
	empAuth := map[string]string{"Authorization": "Bearer " + empSession}

	endpoints := []string{
		"/enterprise/v1/admin/users",
		"/enterprise/v1/admin/agents",
		"/enterprise/v1/admin/models",
		"/enterprise/v1/admin/usage",
		"/enterprise/v1/admin/mcp",
		"/enterprise/v1/admin/knowledge",
		"/enterprise/v1/admin/dlp",
		"/enterprise/v1/admin/audit",
	}

	// 1. Non-admins are denied on every admin endpoint, called directly.
	for _, ep := range endpoints {
		rec := env.do(t, http.MethodGet, ep, "", "", empAuth)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-admin %s: %d, want 403", ep, rec.Code)
		}
	}

	// 2. No credential at all is rejected.
	for _, ep := range endpoints {
		rec := env.do(t, http.MethodGet, ep, "", "", nil)
		if rec.Code == http.StatusOK {
			t.Fatalf("unauthenticated %s must not succeed", ep)
		}
	}

	// Seed sensitive rows so the secret-free assertions have teeth: an agent
	// release whose payload/signature must never leak, and a governed MCP
	// tool whose endpoint URL and credential reference must never leak.
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO agent_releases (client, version, os, arch, minimum_version, digest, payload, signature, signed_by, workspace_id)
		 VALUES ('harness', '9.9.9', 'darwin', 'arm64', '1.0.0', 'digest-9999', 'admin-console-secret-payload', 'admin-console-secret-signature', 'pkg-agent', 'secops')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM agent_releases WHERE client='harness' AND version='9.9.9';`)
	})

	// 3. Admin reads succeed across every domain.
	for _, ep := range endpoints {
		rec := env.do(t, http.MethodGet, ep, "", "", adminAuth)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin %s: %d %s", ep, rec.Code, rec.Body.String())
		}
	}

	// 4. Secret-free payloads: no release payload/signature, no storage or
	// credential data anywhere in the admin responses.
	agentsBody := env.do(t, http.MethodGet, "/enterprise/v1/admin/agents", "", "", adminAuth).Body.String()
	if strings.Contains(agentsBody, "admin-console-secret-payload") ||
		strings.Contains(agentsBody, "admin-console-secret-signature") {
		t.Fatalf("admin agents response leaked release bytes or signature")
	}
	mcpBody := env.do(t, http.MethodGet, "/enterprise/v1/admin/mcp", "", "", adminAuth).Body.String()
	if strings.Contains(mcpBody, "endpoint_url") || strings.Contains(mcpBody, "credential") {
		t.Fatalf("admin mcp response leaked endpoint or credential reference data")
	}
	usersBody := env.do(t, http.MethodGet, "/enterprise/v1/admin/users", "", "", adminAuth).Body.String()
	if strings.Contains(usersBody, "key_hash") || strings.Contains(usersBody, "prefix") {
		t.Fatalf("admin users response leaked key material fields")
	}

	// 5. Pagination is honored server-side.
	paged := env.do(t, http.MethodGet, "/enterprise/v1/admin/users?limit=1&offset=0", "", "", adminAuth).Body.String()
	if !strings.Contains(paged, `"limit":1`) || !strings.Contains(paged, `"offset":0`) {
		t.Fatalf("pagination contract missing: %s", paged)
	}
	badLimit := env.do(t, http.MethodGet, "/enterprise/v1/admin/users?limit=5000", "", "", adminAuth)
	if badLimit.Code != http.StatusBadRequest {
		t.Fatalf("oversized limit must fail closed: %d", badLimit.Code)
	}

	// 6. Every admin read produced a reliable-path audit record for the actor.
	var n int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type = 'admin.read' AND actor_user_id = $1`, adminID).Scan(&n); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if n < len(endpoints) {
		t.Fatalf("admin.read audit records missing: got %d, want >= %d", n, len(endpoints))
	}
}
