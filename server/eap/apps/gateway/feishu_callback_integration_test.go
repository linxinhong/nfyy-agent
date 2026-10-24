//go:build integration

// Feishu callback journey (TASK-013): the gateway Feishu profile drives the
// real handler chain against a fake Feishu IdP — state single-use enforcement,
// identity exchange, canonical Principal persistence, workspace mapping
// through the operator-declared groups, reliable-path audit and one employee
// key created by the same Principal.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/oidc"
)

func TestFeishuCallbackJourney(t *testing.T) {
	// Fake Feishu IdP: token exchange + user_info.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open-apis/auth/v3/app_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["app_id"] != "cli_e2e" || body["app_secret"] != "e2e-secret" {
			_, _ = w.Write([]byte(`{"code":99991661,"msg":"invalid app credential","data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","app_access_token":"app-at","expire":7200}`))
	})
	mux.HandleFunc("POST /open-apis/authen/v1/oidc/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer app-at" {
			_, _ = w.Write([]byte(`{"code":20014,"msg":"invalid app access token","data":null}`))
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "real-code" {
			_, _ = w.Write([]byte(`{"code":20003,"msg":"invalid code","data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"access_token":"u-at","expires_in":6900}}`))
	})
	mux.HandleFunc("GET /open-apis/authen/v1/user_info", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer u-at" {
			_, _ = w.Write([]byte(`{"code":99991661,"msg":"invalid access token","data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"union_id":"on_feishu_user","open_id":"ou_app","name":"飞书用户","email":"feishu-user@example.com"}}`))
	})
	idp := httptest.NewServer(mux)
	defer idp.Close()

	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.OIDCFeishu = &oidc.FeishuConfig{
		ClientID: "cli_e2e", ClientSecret: "e2e-secret",
		RedirectURL:   "http://127.0.0.1:18080/enterprise/v1/auth/oidc/callback",
		DefaultGroups: []string{"secops"},
		AppTokenURL:   idp.URL + "/open-apis/auth/v3/app_access_token/internal",
		TokenURL:      idp.URL + "/open-apis/authen/v1/oidc/access_token",
		UserInfoURL:   idp.URL + "/open-apis/authen/v1/user_info",
	}
	env.handler = env.srv.Handler()

	// Operator-declared IdP group -> workspace mapping (out of band).
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO idp_group_workspaces (idp_group, workspace_id) VALUES ('secops','secops')
		 ON CONFLICT (idp_group, workspace_id) DO NOTHING`); err != nil {
		t.Fatalf("seed group mapping: %v", err)
	}

	// 1. Login redirect carries the Feishu authorize contract.
	rec := env.do(t, http.MethodGet, "/enterprise/v1/auth/oidc/login", "", "", nil)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "accounts.feishu.cn") &&
		!strings.Contains(rec.Header().Get("Location"), "authorize") {
		t.Fatalf("login redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var state string
	if err := env.pool.QueryRow(ctx, `SELECT state FROM oidc_flow_states LIMIT 1`).Scan(&state); err != nil {
		t.Fatalf("flow state: %v", err)
	}

	// 2. Callback binds the identity, maps the declared group, persists the
	// canonical Principal and establishes the session.
	rec = env.do(t, http.MethodGet,
		"/enterprise/v1/auth/oidc/callback?code=real-code&state="+state, "", "", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	var (
		userID, subject, email string
	)
	if err := env.pool.QueryRow(ctx,
		`SELECT id, email FROM users WHERE email = 'feishu-user@example.com'`).Scan(&userID, &email); err != nil {
		t.Fatalf("canonical Principal row missing: %v", err)
	}
	if err := env.pool.QueryRow(ctx,
		`SELECT idp_subject FROM portal_sessions WHERE user_id = $1 LIMIT 1`, userID).Scan(&subject); err != nil {
		t.Fatalf("portal session missing: %v", err)
	}
	if subject != "on_feishu_user" {
		t.Fatalf("session must bind the feishu union_id subject: %q", subject)
	}
	var members int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM workspace_members WHERE user_id = $1 AND workspace_id = 'secops'`, userID).Scan(&members); err != nil || members != 1 {
		t.Fatalf("declared group must map to secops membership: %d %v", members, err)
	}

	// 3. Successful login audit on the reliable path.
	var n int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE type = 'gateway.auth.succeeded' AND actor_user_id = $1 AND detail->>'credential_type' = 'oidc'`, userID).Scan(&n); err != nil || n < 1 {
		t.Fatalf("oidc success audit missing: %d %v", n, err)
	}

	// 4. The same canonical Principal creates one employee key.
	portalSession := rec.Header().Get("Set-Cookie")
	if portalSession == "" {
		t.Fatalf("login cookie missing")
	}
	var sessionID string
	for _, part := range strings.Split(portalSession, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(part), "eap_session="); ok {
			sessionID = v
		}
	}
	// The Portal bootstraps its memory session from the HttpOnly cookie:
	// the profile issues the control-plane session JWT.
	profRec := env.do(t, http.MethodGet, "/enterprise/v1/auth/oidc/profile", "", "",
		map[string]string{"Cookie": "eap_session=" + sessionID})
	if profRec.Code != http.StatusOK {
		t.Fatalf("oidc profile: %d %s", profRec.Code, profRec.Body.String())
	}
	var profile struct {
		SessionToken string `json:"session_token"`
	}
	if err := json.Unmarshal(profRec.Body.Bytes(), &profile); err != nil || profile.SessionToken == "" {
		t.Fatalf("profile must issue the control-plane session token: %s", profRec.Body.String())
	}
	keyRec := env.do(t, http.MethodPost, "/enterprise/v1/keys", "", `{"name":"feishu-journey"}`,
		map[string]string{"Authorization": "Bearer " + profile.SessionToken})
	if keyRec.Code != http.StatusCreated && keyRec.Code != http.StatusOK {
		t.Fatalf("key creation via the feishu session: %d %s", keyRec.Code, keyRec.Body.String())
	}

	// 5. State is single-use: replaying the callback fails closed.
	rec = env.do(t, http.MethodGet,
		"/enterprise/v1/auth/oidc/callback?code=real-code&state="+state, "", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("state replay must be rejected: %d", rec.Code)
	}
}
