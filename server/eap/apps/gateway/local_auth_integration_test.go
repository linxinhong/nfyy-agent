//go:build integration

// Local account/password login coverage (TASK-027): admin creates the
// account, local login reaches the same canonical Principal (admin role
// honored), wrong password and unknown user are indistinguishable and
// audited, plaintext never lands in the database.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestLocalAuthLogin(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.LocalAuth = true
	env.handler = env.srv.Handler()

	// Admin bootstraps the local account.
	adminSession := mcpLogin(t, env, "local-admin@example.com", "secops")
	adminID := mcpUserID(t, env, "local-admin@example.com")
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspace_members SET roles = '{admin}' WHERE user_id = $1`, adminID); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	adminAuth := map[string]string{"Authorization": "Bearer " + adminSession}

	rec := env.do(t, http.MethodPost, "/enterprise/v1/admin/local-accounts", "",
		`{"username":"lin","password":"s3cret-pass-9","email":"lin@livecn","display_name":"lin","workspace":"secops","roles":["admin","developer"]}`,
		adminAuth)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create local account: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "s3cret") {
		t.Fatalf("response must not echo the password")
	}
	// An existing username is never reset, and other Workspaces are out of reach.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/admin/local-accounts", "",
		`{"username":"lin","password":"takeover-pass-1","email":"x@example.com","workspace":"secops"}`, adminAuth)
	if rec.Code != http.StatusConflict {
		t.Fatalf("existing username must not be reset: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/admin/local-accounts", "",
		`{"username":"elsewhere","password":"another-pass-1","email":"e@example.com","workspace":"other-ws"}`, adminAuth)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign workspace: %d %s", rec.Code, rec.Body.String())
	}

	// 1. Local login reaches the same canonical Principal with admin rights.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/auth/local/login", "",
		`{"username":"lin","password":"s3cret-pass-9"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("local login: %d %s", rec.Code, rec.Body.String())
	}
	var sess struct {
		SessionToken string `json:"session_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if sess.SessionToken == "" {
		t.Fatalf("session token missing")
	}
	var roles string
	if err := env.pool.QueryRow(ctx,
		`SELECT array_to_string(roles, ',') FROM workspace_members
		 WHERE user_id = (SELECT id FROM users WHERE idp_subject = 'local:lin')`).Scan(&roles); err != nil || !strings.Contains(roles, "admin") {
		t.Fatalf("local subject must carry admin via membership: %q %v", roles, err)
	}

	// 2. Wrong password and unknown user: identical 401 shape, both audited.
	for _, body := range []string{
		`{"username":"lin","password":"wrong-password"}`,
		`{"username":"ghost","password":"whatever1"}`,
	} {
		rec = env.do(t, http.MethodPost, "/enterprise/v1/auth/local/login", "", body, nil)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid username or password") {
			t.Fatalf("login failure shape: %d %s", rec.Code, rec.Body.String())
		}
	}

	// 3. Plaintext password never lands in the database.
	var n int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM local_credentials WHERE password_hash LIKE '%s3cret%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext password found in storage: %d %v", n, err)
	}

	// 4. Failed and succeeded logins audited with the local credential type.
	var failed int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type='gateway.auth.failed' AND outcome='denied'
		 AND detail->>'credential_type'='local_password'`).Scan(&failed); err != nil {
		t.Fatalf("failed audit query: %v", err)
	}
	if failed < 2 {
		t.Fatalf("failed-login audits missing: %d", failed)
	}
	var okd int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type='gateway.auth.succeeded' AND outcome='allowed'
		 AND detail->>'credential_type'='local_password'`).Scan(&okd); err != nil || okd < 1 {
		t.Fatalf("success audit missing: %d %v", okd, err)
	}
}
