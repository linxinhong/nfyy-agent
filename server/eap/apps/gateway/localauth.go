package main

// Built-in local account/password login (TASK-027, ADR-0021): an
// operator-managed fallback for employees without an OIDC identity. Shares
// the canonical Principal, session issuance and admin role model; the
// primary enterprise path remains OIDC. Gated by EAP_LOCAL_AUTH.

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/rbac"
	"enterprise-agent-platform/packages/store"
	"enterprise-agent-platform/packages/token"

	enterprise "enterprise-agent-platform/packages/protocol-enterprise"

	"golang.org/x/crypto/bcrypt"
)

// handleLocalLogin implements POST /enterprise/v1/auth/local/login:
// username/password -> the same control-plane session JWT as dev-login.
// Unknown user and wrong password are indistinguishable; failures are
// audited on the reliable path.
func (s *Server) handleLocalLogin(w http.ResponseWriter, r *http.Request) {
	if !s.LocalAuth || s.Store == nil {
		writeProblem(w, r, errs.New(errs.NotFound, "local login is not available"))
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	body.Username = strings.TrimSpace(strings.ToLower(body.Username))
	if body.Username == "" || body.Password == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "username and password are required"))
		return
	}

	deny := func(reason string) {
		ev := audit.NewEvent(audit.EventAuthFailed, audit.OutcomeDenied)
		ev.RequestID = requestIDOf(r)
		ev.TraceID = traceIDOf(r)
		ev.Detail = map[string]any{"reason": reason, "credential_type": "local_password"}
		if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
			log.Printf("gateway: SECURITY AUDIT delivery failure: %v", err)
		}
		// Identical shape/timing posture for every failure class.
		writeProblem(w, r, errs.New(errs.Unauthenticated, "invalid username or password"))
	}

	userID, hash, err := s.Store.GetLocalCredential(r.Context(), body.Username)
	if err != nil {
		deny("unknown_user_or_lookup_failed")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
		deny("bad_password")
		return
	}
	// A one-time password proves the credential but never yields a session:
	// the caller must replace it via POST /enterprise/v1/auth/local/password.
	mustChange, err := s.Store.LocalPasswordChangeRequired(r.Context(), body.Username)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "credential state lookup failed"))
		return
	}
	if mustChange {
		writeJSON(w, http.StatusOK, map[string]any{"password_change_required": true})
		return
	}
	user, err := s.Store.GetUser(r.Context(), userID)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "user lookup failed"))
		return
	}

	session, err := s.Signer.Sign(token.IssueInput{
		Session:  identity.Session{Principal: identity.Principal{UserID: user.ID, OrganizationID: user.OrganizationID, DepartmentID: user.DepartmentID}},
		Audience: token.AudienceAgentGateway,
		TTL:      token.MaxTTL,
		Now:      s.now(),
	})
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "session issuance failed"))
		return
	}

	ev := audit.NewEvent(audit.EventAuthSucceeded, audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: user.ID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Detail = map[string]any{"credential_type": "local_password"}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		log.Printf("gateway: SECURITY AUDIT delivery failure: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"session_token": session,
		"token_type":    enterprise.TokenType,
		"expires_in":    int64(token.MaxTTL / time.Second),
	})
}

// ensureDefaultLocalAdmin guarantees the bootstrap admin (admin /
// EAP_LOCAL_ADMIN_PASSWORD, default eap@2026) exists when local auth is
// enabled. Idempotent: a credential that already exists — including one
// whose password the operator changed — is never reset. The placeholder
// email is an explicit operator default for this bootstrap account only.
// When the password-change endpoint is served (Members wired), the
// bootstrap password is one-time: the first login must replace it, so a
// deployment never keeps the shipped default.
func (s *Server) ensureDefaultLocalAdmin(ctx context.Context, adminPassword string) error {
	_, _, err := s.Store.GetLocalCredential(ctx, "admin")
	if err == nil {
		return nil // already provisioned (possibly with a changed password)
	}
	user, err := s.Store.UpsertUser(ctx, store.IDClaims{
		Subject: "local:admin", Email: "admin@eap.local", DisplayName: "Platform Admin",
		OrganizationID: "org_default", DepartmentID: "dept_platform",
	})
	if err != nil {
		return err
	}
	if err := s.Store.SyncMemberships(ctx, user.ID, []string{"secops"}, []string{"admin", "developer"}); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := s.Store.UpsertLocalCredential(ctx, "admin", user.ID, string(hash)); err != nil {
		return err
	}
	if s.Members != nil {
		if err := s.Store.RequireLocalPasswordChange(ctx, "admin"); err != nil {
			return err
		}
	}
	ev := audit.NewEvent("local.bootstrap_admin", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: user.ID}
	ev.Detail = map[string]any{"username": "admin"}
	if err := audit.RequireWrite(ctx, s.Store, ev); err != nil {
		// Bootstrap must not crash-loop the gateway, but the failure is loud.
		log.Printf("gateway: SECURITY AUDIT delivery failure for bootstrap admin: %v", err)
	}
	log.Printf("gateway: default local admin provisioned (username=admin) — change the default password via the admin API")
	return nil
}

// handleAdminCreateLocalAccount implements POST
// /enterprise/v1/admin/local-accounts: create a local login with a
// membership in the admin's own Workspace. Admin role required; the creation
// is audited; the plaintext password is never stored or echoed.
//
// It never touches an existing username: resetting it here would let an
// admin of one Workspace take over (and re-scope) an account of another.
// With Members wired the new password is one-time, like /admin/members.
func (s *Server) handleAdminCreateLocalAccount(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.MembersManage)
	if !ok {
		return
	}
	var body struct {
		Username    string   `json:"username"`
		Password    string   `json:"password"`
		Email       string   `json:"email"`
		DisplayName string   `json:"display_name"`
		Workspace   string   `json:"workspace"`
		Roles       []string `json:"roles"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	body.Username = strings.TrimSpace(strings.ToLower(body.Username))
	body.Email = strings.TrimSpace(body.Email)
	body.Workspace = strings.TrimSpace(body.Workspace)
	if body.Username == "" || len(body.Password) < 8 || body.Email == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument,
			"username, password (min 8 chars) and email are required"))
		return
	}
	if body.Workspace == "" {
		body.Workspace = session.workspace
	}
	if body.Workspace != session.workspace {
		writeProblem(w, r, errs.New(errs.PermissionDenied, "accounts can only be created in your own workspace"))
		return
	}
	if _, _, err := s.Store.GetLocalCredential(r.Context(), body.Username); err == nil {
		writeProblem(w, r, errs.New(errs.Conflict, "username already exists"))
		return
	}
	roles := body.Roles
	if len(roles) == 0 {
		roles = []string{"developer"}
	}

	user, err := s.Store.UpsertUser(r.Context(), store.IDClaims{
		Subject: "local:" + body.Username, Email: body.Email, DisplayName: body.DisplayName,
		OrganizationID: "org_default", DepartmentID: "dept_platform",
	})
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "user sync failed"))
		return
	}
	if err := s.Store.SyncMemberships(r.Context(), user.ID, []string{body.Workspace}, roles); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "membership sync failed"))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "password hashing failed"))
		return
	}
	if err := s.Store.UpsertLocalCredential(r.Context(), body.Username, user.ID, string(hash)); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "local credential persist failed"))
		return
	}
	if s.Members != nil {
		if err := s.Store.RequireLocalPasswordChange(r.Context(), body.Username); err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "local credential persist failed"))
			return
		}
	}

	ev := audit.NewEvent("admin.local_account_created", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.userID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Resource = "local_credentials/" + body.Username
	ev.Detail = map[string]any{"username": body.Username, "user_id": user.ID, "workspace": body.Workspace, "roles": roles}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "admin audit delivery failed — change refused"))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"username": body.Username, "user_id": user.ID, "workspace": body.Workspace, "roles": roles,
	})
}
