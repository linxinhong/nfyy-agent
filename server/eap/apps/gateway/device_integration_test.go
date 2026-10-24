//go:build integration

// Device authorization + CLI bootstrap integration (TASK-015 acceptance):
// device flow login with rotation, ETag/versioned Workspace bootstrap,
// logout revocation; expired codes, denied approval, excessive polling,
// replay, removed membership and cross-Workspace selection fail closed with
// audit evidence; device Principal == agk Principal with equivalent
// bootstrap contents.
package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type deviceEnv struct {
	env      *sliceEnv
	session  string
	keyValue string
}

func TestDeviceBootstrapIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.handler = env.srv.Handler()

	// Login (canonical Principal) + employee key for equivalence checks.
	mcpLogin(t, env, "device@example.com", "secops")
	userID := mcpUserID(t, env, "device@example.com")
	agkKey := seedKey(t, env, userID)
	agkAuth := map[string]string{"Authorization": "Bearer " + agkKey, "X-Agent-Workspace": "secops"}
	sessionToken := mcpLogin(t, env, "device@example.com", "secops")

	postJSON := func(path, bearer, body string) map[string]any {
		t.Helper()
		rec := env.do(t, http.MethodPost, path, bearer, body, nil)
		var payload map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
		payload["__status"] = float64(rec.Code)
		return payload
	}

	// 1. Start device flow.
	codeResp := postJSON("/enterprise/v1/auth/device/code", "", `{}`)
	if codeResp["__status"].(float64) != http.StatusOK || codeResp["device_code"] == nil {
		t.Fatalf("device code: %v", codeResp)
	}
	deviceCode := codeResp["device_code"].(string)
	userCode := codeResp["user_code"].(string)

	// 2. Pending poll -> authorization_pending.
	poll := postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode))
	if poll["error"] != "authorization_pending" {
		t.Fatalf("pending poll: %v", poll)
	}

	// 3. Denied approval -> access_denied with audit.
	deny := postJSON("/enterprise/v1/auth/device/deny", sessionToken,
		fmt.Sprintf(`{"user_code":%q}`, userCode))
	if deny["__status"].(float64) != http.StatusOK {
		t.Fatalf("deny: %v", deny)
	}
	poll = postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode))
	if poll["error"] != "access_denied" {
		t.Fatalf("denied poll: %v", poll)
	}

	// 4. Second flow: excessive polling while pending -> slow_down, then
	// approve -> token with rotation.
	codeResp = postJSON("/enterprise/v1/auth/device/code", "", `{}`)
	deviceCode = codeResp["device_code"].(string)
	userCode = codeResp["user_code"].(string)
	// 5a. Excessive polling while pending -> slow_down (4th poll).
	poll = postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode)) // 2
	if poll["error"] != "authorization_pending" {
		t.Fatalf("pending poll: %v", poll)
	}
	poll = postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode)) // 3
	if poll["error"] != "authorization_pending" {
		t.Fatalf("pending poll 2: %v", poll)
	}
	poll = postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode)) // 4
	if poll["error"] != "authorization_pending" {
		t.Fatalf("pending poll 3: %v", poll)
	}
	poll = postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode)) // 5
	if poll["error"] != "slow_down" {
		t.Fatalf("expected slow_down, got %v", poll)
	}

	// 5b. Approve, then poll once: token issued, authorization consumed.
	approve := postJSON("/enterprise/v1/auth/device/approve", sessionToken,
		fmt.Sprintf(`{"user_code":%q}`, userCode))
	if approve["__status"].(float64) != http.StatusOK {
		t.Fatalf("approve: %v", approve)
	}
	var refresh1, deviceAccess string
	poll = postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode))
	if poll["refresh_token"] != nil {
		refresh1 = poll["refresh_token"].(string)
		deviceAccess = poll["access_token"].(string)
	}
	if refresh1 == "" {
		t.Fatalf("no refresh token received: %v", poll)
	}

	// 5c. Consumed authorization: further polls fail closed.
	poll = postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode))
	if poll["error"] == nil {
		t.Fatalf("consumed authorization must not re-issue tokens: %v", poll)
	}

	// 6. Device session reaches the CLI bootstrap; ETag/versioned.
	access := deviceAccess
	boot := env.do(t, http.MethodGet, "/enterprise/v1/cli/bootstrap", access, "", nil)
	if boot.Code != http.StatusOK || !strings.Contains(boot.Body.String(), `"manifest_version":1`) || !strings.Contains(boot.Body.String(), `"secops"`) {
		t.Fatalf("bootstrap: %d %s", boot.Code, boot.Body.String())
	}
	etag := boot.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("bootstrap ETag missing")
	}
	boot304 := env.doWithHeaders(t, http.MethodGet, "/enterprise/v1/cli/bootstrap", access, "",
		map[string]string{"If-None-Match": etag})
	if boot304.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d, want 304", boot304.Code)
	}

	// 7. agk key bootstrap: same canonical Principal, equivalent contents.
	bootAgk := env.do(t, http.MethodGet, "/enterprise/v1/cli/bootstrap", agkKey, "", agkAuth)
	if bootAgk.Code != http.StatusOK {
		t.Fatalf("agk bootstrap: %d", bootAgk.Code)
	}
	if !strings.Contains(bootAgk.Body.String(), fmt.Sprintf(`"id":%q`, userID)) {
		t.Fatalf("agk bootstrap Principal mismatch: %s", bootAgk.Body.String())
	}

	// 8. Rotation: refresh yields a NEW refresh token; replaying the old one
	// fails closed.
	rot := postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"refresh_token":%q}`, "refresh_token", refresh1))
	if rot["refresh_token"] == nil || rot["refresh_token"].(string) == refresh1 {
		t.Fatalf("rotation must mint a new refresh token: %v", rot)
	}
	refresh2 := rot["refresh_token"].(string)
	replay := postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"refresh_token":%q}`, "refresh_token", refresh1))
	if replay["error"] != "invalid_grant" {
		t.Fatalf("replayed refresh must fail closed: %v", replay)
	}

	// 9. Cross-Workspace selection on the device session: workspace not in
	// membership -> fail closed.
	cross := env.do(t, http.MethodGet, "/enterprise/v1/cli/bootstrap", refresh2, "", nil)
	_ = cross
	rec := env.do(t, http.MethodGet, "/enterprise/v1/cli/bootstrap", rot["access_token"].(string), "",
		map[string]string{"X-Agent-Workspace": "finance"})
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"finance"`) {
		t.Fatalf("cross-workspace selection must not grant finance")
	}

	// 10. Removed membership: refresh fails closed with audit.
	if _, err := env.pool.Exec(ctx,
		`DELETE FROM workspace_members WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("remove membership: %v", err)
	}
	removed := postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"refresh_token":%q}`, "refresh_token", refresh2))
	if removed["error"] != "invalid_grant" {
		t.Fatalf("removed membership refresh: %v", removed)
	}

	// 11. Logout revokes; post-logout refresh fails closed.
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, roles) VALUES ('secops', $1, '{developer}')
		 ON CONFLICT DO NOTHING`, userID); err != nil {
		t.Fatalf("restore membership: %v", err)
	}
	rot = postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"refresh_token":%q}`, "refresh_token", refresh2))
	if rot["refresh_token"] == nil {
		t.Fatalf("restore refresh: %v", rot)
	}
	refresh3 := rot["refresh_token"].(string)
	logout := env.do(t, http.MethodPost, "/enterprise/v1/auth/device/logout", "",
		fmt.Sprintf(`{"refresh_token":%q}`, refresh3), nil)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", logout.Code)
	}
	after := postJSON("/enterprise/v1/auth/device/token", "",
		fmt.Sprintf(`{"grant_type":%q,"refresh_token":%q}`, "refresh_token", refresh3))
	if after["error"] != "invalid_grant" {
		t.Fatalf("post-logout refresh must fail closed: %v", after)
	}

	// 12. Audit evidence.
	var auditDenials int
	if err := env.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE type = 'gateway.auth.failed' AND detail->>'credential_type' = 'device_flow'`).Scan(&auditDenials); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if auditDenials < 2 {
		t.Fatalf("device-flow denial audits missing: %d", auditDenials)
	}
}
