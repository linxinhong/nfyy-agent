//go:build integration

// Portal registry integration (TASK-020 acceptance): the server-driven
// catalog endpoint serves the workspace-scoped skill list; review/release/
// revoke stay denied for non-maintainers (server-side, not button-hiding);
// the production build serves dynamic pages with API/static precedence.
package main_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gateway "enterprise-agent-platform/apps/gateway"
)

func TestPortalRegistryIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "portal-skills@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "portal-skills@example.com"))
	authz := map[string]string{"Authorization": "Bearer " + keyValue, "X-Agent-Workspace": "secops"}

	// Seed two catalog entries.
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if _, err := env.pool.Exec(ctx,
			`INSERT INTO skill_packages (name, version, workspace_id, digest, payload, status, published_by)
			 VALUES ('portal-skill', $1, 'secops', 'digest-' || $1, 'bytes', 'released', 'usr_seed')
			 ON CONFLICT DO NOTHING`, v); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// 1. Catalog endpoint: server-driven list, workspace-scoped. Both the
	// employee key and the Portal session JWT are legitimate readers.
	rec := env.do(t, http.MethodGet, "/enterprise/v1/skills", keyValue, "", authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body.String())
	}
	portalSession := mcpLogin(t, env, "portal-skills@example.com", "secops")
	rec = env.do(t, http.MethodGet, "/enterprise/v1/skills", "", "",
		map[string]string{"Authorization": "Bearer " + portalSession, "X-Agent-Workspace": "secops"})
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog via session: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"portal-skill"`) || !strings.Contains(body, "1.1.0") {
		t.Fatalf("catalog payload wrong: %s", body)
	}
	// No payload/storage data in the catalog response.
	if strings.Contains(body, "bytes") || strings.Contains(body, "payload") {
		t.Fatalf("catalog leaked storage data: %s", body)
	}

	// 2. Search filter.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/skills?q=portal", keyValue, "", authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "portal-skill") {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, "/enterprise/v1/skills?q=nomatch-xyz", keyValue, "", authz)
	if strings.Contains(rec.Body.String(), "portal-skill") {
		t.Fatalf("search filter broken")
	}

	// 3. Cross-workspace isolation: finance catalog is empty.
	mcpLogin(t, env, "portal-fin@example.com", "finance")
	finKey := seedKey(t, env, mcpUserID(t, env, "portal-fin@example.com"))
	finAuth := map[string]string{"Authorization": "Bearer " + finKey, "X-Agent-Workspace": "finance"}
	rec = env.do(t, http.MethodGet, "/enterprise/v1/skills", finKey, "", finAuth)
	if strings.Contains(rec.Body.String(), "portal-skill") {
		t.Fatalf("cross-workspace catalog leak")
	}

	// 4. Non-maintainer mutation stays denied server-side (agk key = no
	// control-plane session).
	rec = env.do(t, http.MethodPost, "/enterprise/v1/skills/portal-skill/versions/1.0.0/review", keyValue, `{}`, authz)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("agk review: %d, want 401", rec.Code)
	}

	// 5. Production gateway serves the marketplace page (SPA fallback) and
	// keeps API precedence.
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO signing_keys (kid, private_pkcs8, active) VALUES
		 ('k-portal', decode('0000000000000000000000000000000000000000000000000000000000000001','hex'), true)
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed signing key: %v", err)
	}
	portalSite, siteErr := gateway.NewStaticSite(t.TempDir())
	if siteErr != nil {
		// write a minimal dist for the fallback assertion
		distDir := t.TempDir()
		_ = writeMinimalDist(distDir)
		site, err2 := gateway.NewStaticSite(distDir)
		if err2 != nil {
			t.Fatalf("static site: %v", err2)
		}
		portalSite = site
	}
	env2 := newSliceEnv(t, 1000)
	env2.srv.OIDCStore = env.store
	env2.srv.Web = portalSite
	env2.handler = env2.srv.Handler()
	page := env2.do(t, http.MethodGet, "/portal/marketplace", "", "", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<html") {
		t.Fatalf("marketplace SPA fallback: %d", page.Code)
	}
	apiProbe := env2.do(t, http.MethodGet, "/enterprise/v1/skills", keyValue, "", authz)
	if apiProbe.Code != http.StatusUnauthorized {
		t.Fatalf("api precedence lost: %d", apiProbe.Code)
	}
}

func writeMinimalDist(dir string) error {
	return os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><html><body>portal</body></html>"), 0o644)
}
