//go:build integration

// Group-to-role binding coverage (TASK-025): mapped roles reach
// workspace_members, the sync is additive (out-of-band grants survive
// re-login), and removing a binding revokes that workspace on the next
// login while still-mapped workspaces persist.

package main_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/oidc"
)

func TestGroupRoleBindings(t *testing.T) {
	// Minimal Feishu IdP stand-in: fixed identity with an asserted email.
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app_access_token/internal"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","app_access_token":"app-at","expire":7200}`))
		case strings.HasSuffix(r.URL.Path, "/oidc/access_token"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"access_token":"u-at","expires_in":3600}}`))
		case strings.HasSuffix(r.URL.Path, "/user_info"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"union_id":"on_roles","open_id":"ou_roles","name":"角色用户","email":"roles@example.com"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()

	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.OIDCFeishu = &oidc.FeishuConfig{
		ClientID: "cli_roles", ClientSecret: "roles-secret",
		RedirectURL:   "http://127.0.0.1:18080/enterprise/v1/auth/oidc/callback",
		DefaultGroups: []string{"g-admin", "g-dev"},
		AppTokenURL:   idp.URL + "/open-apis/auth/v3/app_access_token/internal",
		TokenURL:      idp.URL + "/open-apis/authen/v1/oidc/access_token",
		UserInfoURL:   idp.URL + "/open-apis/authen/v1/user_info",
	}
	env.handler = env.srv.Handler()

	login := func() (int, string) {
		rec := env.do(t, http.MethodGet, "/enterprise/v1/auth/oidc/login", "", "", nil)
		if rec.Code != http.StatusFound {
			t.Fatalf("login: %d", rec.Code)
		}
		parts := strings.Split(rec.Header().Get("Location"), "state=")
		state := parts[len(parts)-1]
		rec = env.do(t, http.MethodGet,
			"/enterprise/v1/auth/oidc/callback?code=any&state="+state, "", "", nil)
		return rec.Code, rec.Body.String()
	}
	roles := func(ws string) []string {
		var out []string
		if err := env.pool.QueryRow(ctx,
			`SELECT roles FROM workspace_members WHERE user_id = (SELECT id FROM users WHERE email='roles@example.com') AND workspace_id = $1`,
			ws).Scan(&out); err != nil {
			return nil
		}
		return out
	}
	seed := func(group, ws string, roles []string) {
		if _, err := env.pool.Exec(ctx,
			`INSERT INTO idp_group_workspaces (idp_group, workspace_id, roles) VALUES ($1, $2, $3)
			 ON CONFLICT (idp_group, workspace_id) DO UPDATE SET roles = EXCLUDED.roles`,
			group, ws, roles); err != nil {
			t.Fatalf("seed mapping: %v", err)
		}
	}
	drop := func(group, ws string) {
		if _, err := env.pool.Exec(ctx,
			`DELETE FROM idp_group_workspaces WHERE idp_group = $1 AND workspace_id = $2`, group, ws); err != nil {
			t.Fatalf("drop mapping: %v", err)
		}
	}

	// 1. Mapped roles flow into workspace_members per workspace.
	seed("g-admin", "secops", []string{"admin"})
	seed("g-dev", "secops", []string{"developer"})
	seed("g-dev", "finance", []string{"developer"})
	if code, body := login(); code != http.StatusFound {
		t.Fatalf("first login: %d %s", code, body)
	}
	if got := roles("secops"); len(got) != 2 {
		t.Fatalf("secops roles must union across groups: %v", got)
	}
	if got := roles("finance"); len(got) != 1 || got[0] != "developer" {
		t.Fatalf("finance roles: %v", got)
	}

	// 2. An out-of-band grant survives re-login (roles never downgrade).
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspace_members SET roles = '{admin,developer,auditor}'
		 WHERE workspace_id='secops' AND user_id = (SELECT id FROM users WHERE email='roles@example.com')`); err != nil {
		t.Fatal(err)
	}
	if code, _ := login(); code != http.StatusFound {
		t.Fatalf("re-login: %d", code)
	}
	got := roles("secops")
	joined := strings.Join(got, ",")
	for _, want := range []string{"admin", "developer", "auditor"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("out-of-band grant must survive: %v", got)
		}
	}

	// 3. Removing the g-admin binding does NOT downgrade roles inside the
	// still-mapped workspace (documented additive semantics: intra-workspace
	// role revocation is an admin-surface operation). The workspace itself
	// persists because g-dev still maps it.
	drop("g-admin", "secops")
	if code, _ := login(); code != http.StatusFound {
		t.Fatalf("login after unmap: %d", code)
	}
	got = roles("secops")
	joined = strings.Join(got, ",")
	for _, want := range []string{"admin", "developer", "auditor"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("additive sync must preserve all granted roles: %v", got)
		}
	}

	// 4. Removing every binding for finance revokes that membership.
	drop("g-dev", "finance")
	if code, _ := login(); code != http.StatusFound {
		t.Fatalf("login after full unmap: %d", code)
	}
	if got := roles("finance"); got != nil {
		t.Fatalf("fully unmapped workspace must be revoked: %v", got)
	}
	if roles("secops") == nil {
		t.Fatalf("still-mapped workspace must persist")
	}
}
