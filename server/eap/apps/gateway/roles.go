package main

// Role administration and the caller's own permissions (packages/rbac).
//
// Why: roles are Workspace data an admin edits, so the Portal needs the
// catalog (built-ins merged with edits) and each member's effective
// permissions to decide what to show; the gateway still re-checks every
// action server-side.
//
// Invariants: role writes need roles.manage and commit their audit event in
// the store transaction; a member may only create or edit a role whose
// permissions they hold themselves, so roles.manage cannot mint broader
// rights than its holder has.

import (
	"net/http"
	"strings"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/rbac"
)

func removed(before, after []string) []string {
	keep := map[string]bool{}
	for _, k := range after {
		keep[k] = true
	}
	out := []string{}
	for _, k := range before {
		if !keep[k] {
			out = append(out, k)
		}
	}
	return out
}

func nonNilRoles(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// handleAdminListRoles implements GET /enterprise/v1/admin/roles.
func (s *Server) handleAdminListRoles(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	catalog, err := s.Members.Roles(r.Context(), session.workspace)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "role listing failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": catalog, "permissions": rbac.Permissions})
}

// handleAdminPutRole implements PUT /enterprise/v1/admin/roles/{key}: create
// a custom role or edit one; a built-in role's scope and key stay fixed.
func (s *Server) handleAdminPutRole(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.RolesManage)
	if !ok {
		return
	}
	var body struct {
		Name        string            `json:"name"`
		Scope       string            `json:"scope"`
		Permissions []rbac.Permission `json:"permissions"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	role := rbac.Role{Key: strings.TrimSpace(r.PathValue("key")), Name: body.Name, Scope: body.Scope, Permissions: body.Permissions}
	if !session.grants.Covers(role) {
		writeProblem(w, r, errs.New(errs.PermissionDenied, "a role may only grant permissions you hold"))
		return
	}
	ev := s.adminEvent(r, session, "admin.role_saved", "workspace-role/"+role.Key,
		map[string]any{"key": role.Key, "scope": role.Scope, "permissions": role.Permissions})
	saved, err := s.Members.PutRole(r.Context(), session.workspace, role, ev)
	if err != nil {
		writeProblem(w, r, storeProblem(err, "role"))
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleAdminDeleteRole implements DELETE /enterprise/v1/admin/roles/{key}:
// deletes a custom role (and its assignments) or resets a built-in one.
func (s *Server) handleAdminDeleteRole(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.RolesManage)
	if !ok {
		return
	}
	key := r.PathValue("key")
	catalog, err := s.Members.Roles(r.Context(), session.workspace)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "role lookup failed"))
		return
	}
	if role, ok := rbac.Find(catalog, key); ok && !session.grants.Covers(role) {
		writeProblem(w, r, errs.New(errs.PermissionDenied, "removing a role needs every permission it grants"))
		return
	}
	ev := s.adminEvent(r, session, "admin.role_deleted", "workspace-role/"+key, map[string]any{"key": key})
	if err := s.Members.DeleteRole(r.Context(), session.workspace, key, ev); err != nil {
		writeProblem(w, r, storeProblem(err, "role"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMyPermissions implements GET /enterprise/v1/me/permissions: what the
// caller may do in the selected Workspace, for showing the right pages.
func (s *Server) handleMyPermissions(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireControlPlane(w, r)
	if !ok {
		return
	}
	workspace, team := session.grants.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace": session.workspace, "roles": nonNilRoles(session.roles),
		"permissions": workspace, "group_id": session.grants.GroupID, "team_permissions": team,
	})
}
