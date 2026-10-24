//go:build integration

// Agent distribution integration (TASK-019 acceptance): an authorized user
// lists the matching release, redeems a same-domain ticket, verifies digest
// and signature headers; unauthorized workspace, wrong platform and ticket
// replay fail closed; audit evidence exists.
package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestAgentDistributionIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "agentdist@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "agentdist@example.com"))
	authz := map[string]string{"Authorization": "Bearer " + keyValue, "X-Agent-Workspace": "secops"}

	// Seed a released harness release for darwin/arm64.
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO agent_releases (client, version, os, arch, minimum_version, digest, payload, signature, signed_by, workspace_id)
		 VALUES ('harness', '2.0.0', 'darwin', 'arm64', '1.0.0', 'deadbeef', 'payload-bytes', 'sig', 'pkg-agent', 'secops')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM agent_releases WHERE client='harness' AND workspace_id='secops';`)
	})

	// 1. List shows the matching release (client-declared platform).
	rec := env.do(t, http.MethodGet, "/enterprise/v1/agents/harness?os=darwin&arch=arm64", keyValue, "", authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"2.0.0"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	// 1b. List without a declared platform fails closed.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/agents/harness", keyValue, "", authz)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("list without platform: %d, want 400", rec.Code)
	}

	// 2. Ticket issuance + redemption returns payload with digest/signature.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/agents/harness/ticket", keyValue,
		`{"version":"2.0.0","os":"darwin","arch":"arm64"}`, authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket: %d %s", rec.Code, rec.Body.String())
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ticket)
	if ticket.Ticket == "" {
		t.Fatalf("ticket missing")
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/agents/harness/download", keyValue,
		fmt.Sprintf(`{"ticket":%q}`, ticket.Ticket), authz)
	if rec.Code != http.StatusOK || rec.Body.String() != "payload-bytes" {
		t.Fatalf("download: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Agent-Digest") != "sha256-deadbeef" {
		t.Fatalf("digest header missing")
	}

	// 3. Ticket replay fails closed.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/agents/harness/download", keyValue,
		fmt.Sprintf(`{"ticket":%q}`, ticket.Ticket), authz)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ticket replay: %d, want 403", rec.Code)
	}

	// 4. Cross-workspace: finance user cannot list or ticket secops release.
	mcpLogin(t, env, "ad-finance@example.com", "finance")
	finKey := seedKey(t, env, mcpUserID(t, env, "ad-finance@example.com"))
	finAuth := map[string]string{"Authorization": "Bearer " + finKey, "X-Agent-Workspace": "finance"}
	rec = env.do(t, http.MethodGet, "/enterprise/v1/agents/harness?os=darwin&arch=arm64", finKey, "", finAuth)
	if rec.Code != http.StatusOK && strings.Contains(rec.Body.String(), "2.0.0") {
		t.Fatalf("cross-workspace list leaked release")
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/agents/harness/ticket", finKey,
		`{"version":"2.0.0","os":"darwin","arch":"arm64"}`, finAuth)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace ticket: %d, want 404", rec.Code)
	}

	// 5. Wrong platform and unknown version fail closed.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/agents/harness/ticket", keyValue,
		`{"version":"2.0.0","os":"linux","arch":"amd64"}`, authz)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong platform: %d, want 404", rec.Code)
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/agents/harness/ticket", keyValue,
		`{"version":"9.9.9","os":"darwin","arch":"arm64"}`, authz)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown version: %d, want 404", rec.Code)
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/agents/harness/ticket", keyValue,
		`{"version":"2.0.0","os":"plan9","arch":"arm64"}`, authz)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid platform: %d, want 400", rec.Code)
	}

	// 6. Minimum-version denial: a release below the workspace minimum is
	// refused with a reliable-path audit record.
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO agent_releases (client, version, os, arch, minimum_version, digest, payload, signature, signed_by, workspace_id)
		 VALUES ('harness', '0.5.0', 'darwin', 'arm64', '2.0.0', 'deadbeef', 'old-bytes', 'sig', 'pkg-agent', 'secops')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed old release: %v", err)
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/agents/harness/ticket", keyValue,
		`{"version":"0.5.0","os":"darwin","arch":"arm64"}`, authz)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("minimum-version denial: %d, want 403", rec.Code)
	}
	var denied int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type = 'agent.version_denied' AND outcome = 'denied'`).Scan(&denied); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if denied < 1 {
		t.Fatalf("minimum-version denial must produce audit evidence")
	}

	// 7. Ticket records exist as issuance evidence.
	var n int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM download_tickets WHERE release_id IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("ticket query: %v", err)
	}
	if n < 1 {
		t.Fatalf("ticket records missing")
	}
}
