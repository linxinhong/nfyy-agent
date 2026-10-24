//go:build integration

// Editable roles: custom Workspace and team roles, no escalation through
// members.manage or roles.manage, the admin role stays locked, deleting a
// custom role strips its assignments, a built-in edit can be reset, team
// roles grant only inside their team, and every role write is audited.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEditableRoles(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.Members = env.store
	env.srv.LocalAuth = true
	env.handler = env.srv.Handler()

	adminSession := mcpLogin(t, env, "roles-admin@example.com", "secops")
	adminID := mcpUserID(t, env, "roles-admin@example.com")
	if _, err := env.pool.Exec(ctx, `UPDATE workspace_members SET roles = '{admin}' WHERE user_id = $1`, adminID); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	staffSession := mcpLogin(t, env, "roles-staff@example.com", "secops")
	staffID := mcpUserID(t, env, "roles-staff@example.com")
	mcpLogin(t, env, "roles-lawyer@example.com", "secops")
	lawyerID := mcpUserID(t, env, "roles-lawyer@example.com")

	put := func(session, key, body string) *httptest.ResponseRecorder {
		return env.do(t, http.MethodPut, "/enterprise/v1/admin/roles/"+key, session, body, nil)
	}

	// Validation: team roles take only team-grantable permissions; the admin
	// role is locked; a scope cannot change.
	if rec := put(adminSession, "team-reviewer", `{"name":"团队审核人","scope":"team","permissions":["members.manage"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("team role with workspace-only permission: %d %s", rec.Code, rec.Body.String())
	}
	if rec := put(adminSession, "admin", `{"name":"超管","scope":"workspace","permissions":[]}`); rec.Code != http.StatusForbidden {
		t.Fatalf("admin role edit: %d %s", rec.Code, rec.Body.String())
	}
	if rec := put(adminSession, "reviewer", `{"name":"审核人","scope":"team","permissions":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("scope change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := put(adminSession, "team-reviewer", `{"name":"团队审核人","scope":"team","permissions":["knowledge.review","skill.review"]}`); rec.Code != http.StatusOK {
		t.Fatalf("custom team role: %d %s", rec.Code, rec.Body.String())
	}
	if rec := put(adminSession, "staff", `{"name":"行政","scope":"workspace","permissions":["members.manage"]}`); rec.Code != http.StatusOK {
		t.Fatalf("custom workspace role: %d %s", rec.Code, rec.Body.String())
	}
	if rec := put(adminSession, "other", `{"name":"行政","scope":"workspace","permissions":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate role name: %d %s", rec.Code, rec.Body.String())
	}

	rec := env.do(t, http.MethodGet, "/enterprise/v1/admin/roles", adminSession, "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"key":"team-reviewer"`) ||
		!strings.Contains(rec.Body.String(), `"key":"lead"`) || !strings.Contains(rec.Body.String(), `"team_grantable":true`) {
		t.Fatalf("role catalog: %d %s", rec.Code, rec.Body.String())
	}

	// A members.manage holder administers members but cannot hand out (or
	// take away) rights they lack, nor edit roles.
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+staffID, adminSession, `{"roles":["staff"]}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("assign staff: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/admin/members", staffSession, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("staff lists members: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+lawyerID, staffSession, `{"roles":["reviewer"]}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("staff grants reviewer: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+adminID, staffSession, `{"roles":["developer"]}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("staff strips admin: %d %s", rec.Code, rec.Body.String())
	}
	if rec := put(staffSession, "staff", `{"name":"行政","scope":"workspace","permissions":["members.manage","roles.manage"]}`); rec.Code != http.StatusForbidden {
		t.Fatalf("staff edits roles: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/admin/dlp/rules", staffSession, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("staff reaches platform admin: %d %s", rec.Code, rec.Body.String())
	}

	// A team role applies inside the member's own team only.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/admin/groups", adminSession, `{"name":"并购组"}`, nil)
	var group struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &group)
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+lawyerID, adminSession, `{"team_roles":["team-reviewer"]}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("team role without team: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+lawyerID, adminSession,
		`{"group_id":"`+group.ID+`","team_roles":["team-reviewer"]}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("assign team role: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/members/"+lawyerID, adminSession, `{"roles":["team-reviewer"]}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("team role as workspace role: %d %s", rec.Code, rec.Body.String())
	}
	lawyerSession := mcpLogin(t, env, "roles-lawyer@example.com", "secops")
	rec = env.do(t, http.MethodGet, "/enterprise/v1/me/permissions", lawyerSession, "", nil)
	var mine struct {
		Permissions     []string `json:"permissions"`
		GroupID         string   `json:"group_id"`
		TeamPermissions []string `json:"team_permissions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &mine)
	if rec.Code != http.StatusOK || len(mine.Permissions) != 0 || mine.GroupID != group.ID ||
		strings.Join(mine.TeamPermissions, ",") != "knowledge.review,skill.review" {
		t.Fatalf("my permissions: %d %s", rec.Code, rec.Body.String())
	}

	// Deleting a custom role removes it from every assignment; resetting a
	// built-in restores its defaults.
	if rec := env.do(t, http.MethodDelete, "/enterprise/v1/admin/roles/team-reviewer", adminSession, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete role: %d %s", rec.Code, rec.Body.String())
	}
	var teamRoles []string
	if err := env.pool.QueryRow(ctx, `SELECT team_roles FROM workspace_group_members WHERE user_id = $1`, lawyerID).Scan(&teamRoles); err != nil || len(teamRoles) != 0 {
		t.Fatalf("deleted role still assigned: %v %v", teamRoles, err)
	}
	if rec := put(adminSession, "reviewer", `{"name":"知识审核","scope":"workspace","permissions":["knowledge.review"]}`); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"name":"知识审核"`) {
		t.Fatalf("edit built-in: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodDelete, "/enterprise/v1/admin/roles/reviewer", adminSession, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("reset built-in: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, "/enterprise/v1/admin/roles", adminSession, "", nil)
	if !strings.Contains(rec.Body.String(), `"name":"审核人"`) {
		t.Fatalf("built-in not reset: %s", rec.Body.String())
	}
	if rec := env.do(t, http.MethodDelete, "/enterprise/v1/admin/roles/admin", adminSession, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("reset admin: %d", rec.Code)
	}

	for _, typ := range []string{"admin.role_saved", "admin.role_deleted"} {
		var n int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE type = $1`, typ).Scan(&n); err != nil || n == 0 {
			t.Fatalf("audit %s missing: %d %v", typ, n, err)
		}
	}
}
