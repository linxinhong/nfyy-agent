//go:build integration

// Directory merge: built-in entries plus the Workspace's released Skill
// Registry packages; unreleased or foreign-Workspace packages stay out;
// artifacts need an authenticated member and only manifest-listed archives
// are served.

package main_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestCatalogMergesReleasedRegistrySkills(t *testing.T) {
	env := newAdversarialEnv(t)
	ctx := context.Background()
	advSeedSkillPackage(t, env, "merge-released", "1.0.0", "released")
	advSeedSkillPackage(t, env, "merge-pending", "1.0.0", "pending_review")
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO skill_packages (name, version, workspace_id, digest, payload, status, published_by)
		VALUES ('merge-foreign', '1.0.0', 'finance', 'd', '\x00', 'released', $1)`, env.userID); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}
	ws := map[string]string{"X-Agent-Workspace": "secops"}

	rec := env.do(t, http.MethodGet, "/enterprise/v1/catalog/search?q=merge", env.userSession, "", ws)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"registry:merge-released@1.0.0"`) ||
		strings.Contains(body, "merge-pending") || strings.Contains(body, "merge-foreign") {
		t.Fatalf("merged search: %d %s", rec.Code, body)
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/catalog/items/registry.merge-released", env.keyValue, "", ws); rec.Code != http.StatusOK {
		t.Fatalf("registry item via key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/catalog/search", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous directory: %d", rec.Code)
	}
	rec = env.do(t, http.MethodGet, "/enterprise/v1/capability-packages/builtin.python.echo/artifacts/"+advCatalogArchive, env.keyValue, "", ws)
	if rec.Code != http.StatusOK || rec.Body.String() != "signed-fixture" {
		t.Fatalf("artifact: %d %q", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/capability-packages/builtin.python.echo/artifacts/manifest.json", env.keyValue, "", ws); rec.Code != http.StatusNotFound {
		t.Fatalf("unlisted file: %d", rec.Code)
	}
}
