//go:build integration

// Workspace member and group administration: admin-only, scoped to the
// admin's own Workspace, one-time password must be replaced before a session
// exists, last admin cannot be removed, removal cascades group rows and
// revokes keys, and every change is audited.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestWorkspaceMemberAdministration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.Members = env.store
	env.srv.LocalAuth = true
	env.handler = env.srv.Handler()

	adminSession := mcpLogin(t, env, "ws-admin@example.com", "secops")
	adminID := mcpUserID(t, env, "ws-admin@example.com")
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspace_members SET roles = '{admin}' WHERE user_id = $1`, adminID); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	plainSession := mcpLogin(t, env, "ws-plain@example.com", "secops")

	// Non-admins are refused on every admin write.
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/admin/groups", plainSession, `{"name":"诉讼组"}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin group create: %d %s", rec.Code, rec.Body.String())
	}

	// Groups.
	rec := env.do(t, http.MethodPost, "/enterprise/v1/admin/groups", adminSession, `{"name":"诉讼组"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", rec.Code, rec.Body.String())
	}
	var group struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &group)
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/admin/groups", adminSession, `{"name":"诉讼组"}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate group: %d", rec.Code)
	}

	// Create a member with a one-time password; the target Workspace comes
	// from the admin session, not the body.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/admin/members", adminSession,
		`{"username":"chen","email":"chen@example.com","display_name":"陈律师","roles":["reviewer"],"group_id":"`+group.ID+`","team_roles":["lead"],"workspace":"finance"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		UserID          string `json:"user_id"`
		Workspace       string `json:"workspace"`
		OneTimePassword string `json:"one_time_password"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Workspace != "secops" || created.OneTimePassword == "" {
		t.Fatalf("member creation response: %s", rec.Body.String())
	}
	var leaked int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM local_credentials WHERE password_hash = $1`,
		created.OneTimePassword).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("plaintext one-time password stored: %d %v", leaked, err)
	}

	// One-time password never yields a session.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/auth/local/login", "",
		`{"username":"chen","password":"`+created.OneTimePassword+`"}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"password_change_required":true`) ||
		strings.Contains(rec.Body.String(), "session_token") {
		t.Fatalf("one-time login: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/auth/local/password", "",
		`{"username":"chen","current_password":"wrong-one","new_password":"chen-new-pass-1"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("password change with wrong current: %d", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/auth/local/password", "",
		`{"username":"chen","current_password":"`+created.OneTimePassword+`","new_password":"chen-new-pass-1"}`, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("password change: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/auth/local/login", "",
		`{"username":"chen","password":"chen-new-pass-1"}`, nil)
	var sess struct {
		SessionToken string `json:"session_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if rec.Code != http.StatusOK || sess.SessionToken == "" {
		t.Fatalf("login after change: %d %s", rec.Code, rec.Body.String())
	}

	// Listing shows roles, group and team roles.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/admin/members", adminSession, "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"group_id":"`+group.ID+`"`) || !strings.Contains(rec.Body.String(), `"team_roles":["lead"]`) {
		t.Fatalf("list members: %d %s", rec.Code, rec.Body.String())
	}

	// Clearing the group also clears its team roles.
	rec = env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+created.UserID, adminSession, `{"group_id":""}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"group_id":""`) || !strings.Contains(rec.Body.String(), `"team_roles":[]`) {
		t.Fatalf("clear group: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+created.UserID, adminSession, `{"group_id":"grp_missing"}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown group: %d", rec.Code)
	}
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+created.UserID, adminSession, `{"roles":["owner"]}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown role: %d", rec.Code)
	}

	// The last admin can neither be demoted nor removed.
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+adminID, adminSession, `{"roles":["developer"]}`, nil); rec.Code != http.StatusConflict {
		t.Fatalf("demote last admin: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodDelete, "/enterprise/v1/admin/members/"+adminID, adminSession, "", nil); rec.Code != http.StatusConflict {
		t.Fatalf("remove last admin: %d", rec.Code)
	}

	// Removal: the member's key stops working and the group row is gone.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/keys", sess.SessionToken, `{"name":"desktop","environment":"live"}`, nil)
	var key struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &key)
	if rec.Code != http.StatusCreated || key.Key == "" {
		t.Fatalf("member key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+created.UserID, adminSession, `{"group_id":"`+group.ID+`"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("regroup: %d", rec.Code)
	}
	if rec := env.do(t, http.MethodDelete, "/enterprise/v1/admin/members/"+created.UserID, adminSession, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("remove member: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/me", key.Key, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("removed member key must fail: %d %s", rec.Code, rec.Body.String())
	}
	var groupRows int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_group_members WHERE user_id = $1`, created.UserID).Scan(&groupRows); err != nil || groupRows != 0 {
		t.Fatalf("group membership must cascade: %d %v", groupRows, err)
	}

	// Deleting a group keeps no dangling rows.
	if rec := env.do(t, http.MethodDelete, "/enterprise/v1/admin/groups/"+group.ID, adminSession, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete group: %d", rec.Code)
	}

	// Every write is audited.
	for _, typ := range []string{"admin.group_created", "admin.member_created", "admin.member_updated",
		"admin.member_removed", "admin.group_deleted", "auth.local_password_changed"} {
		var n int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE type = $1`, typ).Scan(&n); err != nil || n == 0 {
			t.Fatalf("audit %s missing: %d %v", typ, n, err)
		}
	}
	var resource string
	if err := env.pool.QueryRow(ctx, `SELECT resource FROM audit_events WHERE type = 'admin.group_created'`).Scan(&resource); err != nil || resource != "workspace-group/"+group.ID {
		t.Fatalf("group audit resource: %q %v", resource, err)
	}
}
