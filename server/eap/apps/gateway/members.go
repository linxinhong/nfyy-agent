package main

// Workspace member and group administration (docs/knowledge-platform-architecture.md §8.4).
//
// Admins manage the Workspace their control-plane session resolves to; the
// target Workspace is never taken from the request body. Every write commits
// its audit event in the store transaction, so a refused audit refuses the
// change. Local members receive a server-generated one-time password that
// appears once in the creation response and must be replaced through
// POST /enterprise/v1/auth/local/password before login issues a session.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/rbac"
	"enterprise-agent-platform/packages/store"
)

// MemberStore is the narrow persistence surface for member administration.
type MemberStore interface {
	ListMembers(ctx context.Context, workspaceID string) ([]store.WorkspaceMember, error)
	GetMember(ctx context.Context, workspaceID, userID string) (store.WorkspaceMember, error)
	CreateLocalMember(ctx context.Context, in store.NewLocalMember, ev audit.Event) (string, error)
	UpdateMember(ctx context.Context, workspaceID, userID string, u store.MemberUpdate, ev audit.Event) error
	RemoveMember(ctx context.Context, workspaceID, userID string, ev audit.Event) error
	ListGroups(ctx context.Context, workspaceID string) ([]store.WorkspaceGroup, error)
	CreateGroup(ctx context.Context, workspaceID, name string, ev audit.Event) (store.WorkspaceGroup, error)
	RenameGroup(ctx context.Context, workspaceID, groupID, name string, ev audit.Event) error
	DeleteGroup(ctx context.Context, workspaceID, groupID string, ev audit.Event) error
	ChangeLocalPassword(ctx context.Context, username, passwordHash string, ev audit.Event) error
	Roles(ctx context.Context, workspaceID string) ([]rbac.Role, error)
	Grants(ctx context.Context, workspaceID, userID string, roles []string) (rbac.Grants, error)
	PutRole(ctx context.Context, workspaceID string, r rbac.Role, ev audit.Event) (rbac.Role, error)
	DeleteRole(ctx context.Context, workspaceID, key string, ev audit.Event) error
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}$`)

const minPasswordLength = 8

// assignRoles validates role keys against the Workspace catalog at scope and
// checks the assigner holds every permission each role grants (no
// escalation through members.manage). Workspace roles default to the base
// member role; team roles may be empty.
func (s *Server) assignRoles(ctx context.Context, session gatewayIdentities, keys []string, scope string) ([]string, error) {
	if len(keys) == 0 {
		if scope == rbac.ScopeWorkspace {
			keys = []string{rbac.RoleDeveloper}
		} else {
			return []string{}, nil
		}
	}
	catalog, err := s.Members.Roles(ctx, session.workspace)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "role lookup failed")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.ToLower(strings.TrimSpace(key))
		role, ok := rbac.Find(catalog, key)
		if !ok || role.Scope != scope {
			return nil, errs.New(errs.InvalidArgument, "unknown "+scope+" role: "+key)
		}
		if !session.grants.Covers(role) {
			return nil, errs.New(errs.PermissionDenied, "assigning "+key+" needs every permission it grants")
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out, nil
}

// sameRoles reports whether two role-key lists hold the same keys.
func sameRoles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, k := range a {
		set[k] = true
	}
	for _, k := range b {
		if !set[k] {
			return false
		}
	}
	return true
}

func validGroupName(name string) bool {
	n := utf8.RuneCountInString(name)
	return n >= 1 && n <= 80
}

func oneTimePassword() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// storeProblem maps member-store sentinels onto the frozen error contract.
func storeProblem(err error, what string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errs.New(errs.NotFound, what+" not found in this workspace")
	case errors.Is(err, store.ErrConflict):
		return errs.New(errs.Conflict, what+" already exists")
	case errors.Is(err, store.ErrLastAdmin):
		return errs.New(errs.Conflict, "the workspace must keep at least one admin")
	case errors.Is(err, store.ErrGroupInUse):
		return errs.New(errs.Conflict, "the group still scopes team content")
	case errors.Is(err, store.ErrNoGroup):
		return errs.New(errs.InvalidArgument, "team roles need a team (group_id)")
	case errors.Is(err, rbac.ErrLocked):
		return errs.New(errs.PermissionDenied, "the admin role cannot be changed")
	case errors.Is(err, rbac.ErrInvalid):
		return errs.New(errs.InvalidArgument, err.Error())
	default:
		return errs.Wrap(err, errs.Internal, what+" change failed — audit or storage refused")
	}
}

func (s *Server) adminEvent(r *http.Request, session gatewayIdentities, t audit.EventType, resource string, detail map[string]any) audit.Event {
	ev := audit.NewEvent(t, audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.userID}
	ev.WorkspaceID = session.workspace
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Resource = resource
	ev.Detail = detail
	return ev
}

func memberJSON(m store.WorkspaceMember) map[string]any {
	return map[string]any{
		"user_id": m.UserID, "email": m.Email, "display_name": m.DisplayName,
		"username": m.Username, "roles": m.Roles, "group_id": m.GroupID, "team_roles": nonNilRoles(m.TeamRoles),
	}
}

func groupJSON(g store.WorkspaceGroup) map[string]any {
	return map[string]any{"id": g.ID, "name": g.Name, "members": g.Members}
}

// handleAdminListMembers implements GET /enterprise/v1/admin/members.
func (s *Server) handleAdminListMembers(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	members, err := s.Members.ListMembers(r.Context(), session.workspace)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "member listing failed"))
		return
	}
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		out = append(out, memberJSON(m))
	}
	if err := s.auditAdminRead(r, session, "admin.members"); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "admin audit delivery failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": session.workspace, "members": out})
}

// handleAdminCreateMember implements POST /enterprise/v1/admin/members.
func (s *Server) handleAdminCreateMember(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	var body struct {
		Username    string   `json:"username"`
		Email       string   `json:"email"`
		DisplayName string   `json:"display_name"`
		Roles       []string `json:"roles"`
		GroupID     string   `json:"group_id"`
		TeamRoles   []string `json:"team_roles"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	body.Username = strings.ToLower(strings.TrimSpace(body.Username))
	body.Email = strings.TrimSpace(body.Email)
	body.DisplayName = strings.TrimSpace(body.DisplayName)
	body.GroupID = strings.TrimSpace(body.GroupID)
	if !usernamePattern.MatchString(body.Username) || body.DisplayName == "" || utf8.RuneCountInString(body.DisplayName) > 80 {
		writeProblem(w, r, errs.New(errs.InvalidArgument,
			"username (2-63 chars: a-z 0-9 . _ -) and display_name (1-80 chars) are required"))
		return
	}
	if len(body.TeamRoles) > 0 && body.GroupID == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "team_roles requires group_id"))
		return
	}
	roles, err := s.assignRoles(r.Context(), session, body.Roles, rbac.ScopeWorkspace)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	teamRoles, err := s.assignRoles(r.Context(), session, body.TeamRoles, rbac.ScopeTeam)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	password, err := oneTimePassword()
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "password generation failed"))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "password hashing failed"))
		return
	}
	ev := s.adminEvent(r, session, "admin.member_created", "local_credentials/"+body.Username,
		map[string]any{"username": body.Username, "roles": roles, "group_id": body.GroupID, "team_roles": teamRoles})
	userID, err := s.Members.CreateLocalMember(r.Context(), store.NewLocalMember{
		WorkspaceID: session.workspace, Username: body.Username, Email: body.Email,
		DisplayName: body.DisplayName, Roles: roles, GroupID: body.GroupID, TeamRoles: teamRoles,
		PasswordHash: string(hash),
	}, ev)
	if err != nil {
		writeProblem(w, r, storeProblem(err, "member"))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"user_id": userID, "username": body.Username, "workspace": session.workspace,
		"roles": roles, "group_id": body.GroupID, "team_roles": teamRoles,
		"one_time_password": password, "password_change_required": true,
	})
}

// handleAdminUpdateMember implements PATCH /enterprise/v1/admin/members/{userID}.
func (s *Server) handleAdminUpdateMember(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	userID := r.PathValue("userID")
	var body struct {
		Roles     *[]string `json:"roles"`
		GroupID   *string   `json:"group_id"`
		TeamRoles *[]string `json:"team_roles"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	current, err := s.Members.GetMember(r.Context(), session.workspace, userID)
	if err != nil {
		writeProblem(w, r, storeProblem(err, "member"))
		return
	}
	update := store.MemberUpdate{}
	detail := map[string]any{"user_id": userID}
	// Removing a role someone holds is as strong as granting it: both need
	// every permission the role grants.
	if body.Roles != nil {
		roles, err := s.assignRoles(r.Context(), session, *body.Roles, rbac.ScopeWorkspace)
		if err == nil && !sameRoles(roles, current.Roles) {
			_, err = s.assignRoles(r.Context(), session, removed(current.Roles, roles), rbac.ScopeWorkspace)
		}
		if err != nil {
			writeProblem(w, r, err)
			return
		}
		update.Roles = &roles
		detail["roles"] = roles
	}
	if body.TeamRoles != nil {
		teamRoles, err := s.assignRoles(r.Context(), session, *body.TeamRoles, rbac.ScopeTeam)
		if err == nil {
			_, err = s.assignRoles(r.Context(), session, removed(current.TeamRoles, teamRoles), rbac.ScopeTeam)
		}
		if err != nil {
			writeProblem(w, r, err)
			return
		}
		update.TeamRoles = &teamRoles
		detail["team_roles"] = teamRoles
	}
	if body.GroupID != nil {
		trimmed := strings.TrimSpace(*body.GroupID)
		update.GroupID = &trimmed
		detail["group_id"] = trimmed
	}
	if update.Roles == nil && update.GroupID == nil && update.TeamRoles == nil {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "nothing to update"))
		return
	}
	ev := s.adminEvent(r, session, "admin.member_updated", "user/"+userID, detail)
	if err := s.Members.UpdateMember(r.Context(), session.workspace, userID, update, ev); err != nil {
		writeProblem(w, r, storeProblem(err, "member or group"))
		return
	}
	m, err := s.Members.GetMember(r.Context(), session.workspace, userID)
	if err != nil {
		writeProblem(w, r, storeProblem(err, "member"))
		return
	}
	writeJSON(w, http.StatusOK, memberJSON(m))
}

// handleAdminRemoveMember implements DELETE /enterprise/v1/admin/members/{userID}.
func (s *Server) handleAdminRemoveMember(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	userID := r.PathValue("userID")
	ev := s.adminEvent(r, session, "admin.member_removed", "user/"+userID, map[string]any{"user_id": userID})
	if err := s.Members.RemoveMember(r.Context(), session.workspace, userID, ev); err != nil {
		writeProblem(w, r, storeProblem(err, "member"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminListGroups implements GET /enterprise/v1/admin/groups.
func (s *Server) handleAdminListGroups(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	groups, err := s.Members.ListGroups(r.Context(), session.workspace)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "group listing failed"))
		return
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupJSON(g))
	}
	if err := s.auditAdminRead(r, session, "admin.groups"); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "admin audit delivery failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": session.workspace, "groups": out})
}

func decodeGroupName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return "", false
	}
	name := strings.TrimSpace(body.Name)
	if !validGroupName(name) {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "name must be 1-80 characters"))
		return "", false
	}
	return name, true
}

// handleAdminCreateGroup implements POST /enterprise/v1/admin/groups.
func (s *Server) handleAdminCreateGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	name, ok := decodeGroupName(w, r)
	if !ok {
		return
	}
	ev := s.adminEvent(r, session, "admin.group_created", "", map[string]any{"name": name})
	g, err := s.Members.CreateGroup(r.Context(), session.workspace, name, ev)
	if err != nil {
		writeProblem(w, r, storeProblem(err, "group"))
		return
	}
	writeJSON(w, http.StatusCreated, groupJSON(g))
}

// handleAdminRenameGroup implements PATCH /enterprise/v1/admin/groups/{groupID}.
func (s *Server) handleAdminRenameGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	name, ok := decodeGroupName(w, r)
	if !ok {
		return
	}
	groupID := r.PathValue("groupID")
	ev := s.adminEvent(r, session, "admin.group_renamed", "workspace-group/"+groupID, map[string]any{"name": name})
	if err := s.Members.RenameGroup(r.Context(), session.workspace, groupID, name, ev); err != nil {
		writeProblem(w, r, storeProblem(err, "group"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": groupID, "name": name})
}

// handleAdminDeleteGroup implements DELETE /enterprise/v1/admin/groups/{groupID}.
func (s *Server) handleAdminDeleteGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	groupID := r.PathValue("groupID")
	ev := s.adminEvent(r, session, "admin.group_deleted", "workspace-group/"+groupID, nil)
	if err := s.Members.DeleteGroup(r.Context(), session.workspace, groupID, ev); err != nil {
		writeProblem(w, r, storeProblem(err, "group"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLocalPasswordChange implements POST /enterprise/v1/auth/local/password:
// username + current password → new password. It is the only way out of a
// one-time password, so it authenticates by credential, not by session, and
// shares the login's indistinguishable failure shape.
func (s *Server) handleLocalPasswordChange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username        string `json:"username"`
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	body.Username = strings.ToLower(strings.TrimSpace(body.Username))
	if body.Username == "" || body.CurrentPassword == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "username and current_password are required"))
		return
	}
	if len(body.NewPassword) < minPasswordLength || body.NewPassword == body.CurrentPassword {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "new_password must be at least 8 characters and differ from the current one"))
		return
	}
	userID, hash, err := s.Store.GetLocalCredential(r.Context(), body.Username)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.CurrentPassword)) != nil {
		ev := audit.NewEvent(audit.EventAuthFailed, audit.OutcomeDenied)
		ev.RequestID = requestIDOf(r)
		ev.TraceID = traceIDOf(r)
		ev.Detail = map[string]any{"reason": "password_change_rejected", "credential_type": "local_password"}
		if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "audit delivery failed"))
			return
		}
		writeProblem(w, r, errs.New(errs.Unauthenticated, "invalid username or password"))
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "password hashing failed"))
		return
	}
	ev := audit.NewEvent("auth.local_password_changed", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: userID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Resource = "local_credentials/" + body.Username
	if err := s.Members.ChangeLocalPassword(r.Context(), body.Username, string(newHash), ev); err != nil {
		writeProblem(w, r, storeProblem(err, "local credential"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
