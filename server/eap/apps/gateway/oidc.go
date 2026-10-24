package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"time"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/oidc"
	"enterprise-agent-platform/packages/store"
	"enterprise-agent-platform/packages/token"
)

// flowStoreKeeps track of in-flight authorizations (single-use state).
type flowState struct {
	Nonce        string
	CodeVerifier string
	CreatedAt    time.Time
}

// startOIDCLogin implements GET /enterprise/v1/auth/oidc/login:
// creates the state/nonce/PKCE triple, persists it server-side and redirects
// to the IdP. Nothing identity-related is exposed to the browser.
func (s *Server) startOIDCLogin(w http.ResponseWriter, r *http.Request) {
	var redirect string
	switch {
	case s.OIDC != nil:
		req, err := oidc.NewAuthorizationRequest(s.now())
		if err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "flow bootstrap failed"))
			return
		}
		if err := s.persistFlowState(r, req); err != nil {
			writeProblem(w, r, err)
			return
		}
		redirect, err = s.OIDC.AuthorizeURL(req)
		if err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "authorize url build failed"))
			return
		}
	case s.OIDCFeishu != nil:
		req, err := oidc.NewAuthorizationRequest(s.now())
		if err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "flow bootstrap failed"))
			return
		}
		if err := s.persistFlowState(r, req); err != nil {
			writeProblem(w, r, err)
			return
		}
		redirect, err = s.OIDCFeishu.FeishuAuthorizeURL(req)
		if err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "authorize url build failed"))
			return
		}
	default:
		writeProblem(w, r, errs.New(errs.NotFound, "oidc login is not configured"))
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// persistFlowState records the single-use authorization state (shared by the
// standard and Feishu profiles; nonce/PKCE stay unused when the IdP does not
// consume them).
func (s *Server) persistFlowState(r *http.Request, req *oidc.AuthorizationRequest) error {
	if _, err := s.OIDCStore.Pool().Exec(r.Context(), `
		INSERT INTO oidc_flow_states (state, nonce, code_verifier, created_at)
		VALUES ($1, $2, $3, $4)`, req.State, req.Nonce, req.CodeVerifier, req.CreatedAt); err != nil {
		return errs.Wrap(err, errs.Internal, "flow state persist failed")
	}
	return nil
}

// oidcCallback implements GET /enterprise/v1/auth/oidc/callback:
// validates state (single-use), exchanges the code with PKCE, validates the
// ID token (issuer/audience/signature/nonce/expiry), maps claims through the
// canonical Principal boundary, creates the HttpOnly session and audits.
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil && s.OIDCFeishu == nil {
		writeProblem(w, r, errs.New(errs.NotFound, "oidc login is not configured"))
		return
	}
	deny := func(reason string) {
		ev := audit.NewEvent(audit.EventAuthFailed, audit.OutcomeDenied)
		ev.RequestID = requestIDOf(r)
		ev.TraceID = traceIDOf(r)
		ev.Detail = map[string]any{"reason": reason, "credential_type": "oidc"}
		if err := audit.RequireWrite(r.Context(), s.OIDCStore, ev); err != nil {
			log.Printf("gateway: SECURITY AUDIT delivery failure: %v", err)
		}
		writeProblem(w, r, errs.New(errs.Unauthenticated, "login failed"))
	}

	// 1. Single-use state with freshness window.
	var stored flowState
	err := s.OIDCStore.Pool().QueryRow(r.Context(), `
		DELETE FROM oidc_flow_states WHERE state = $1
		RETURNING nonce, code_verifier, created_at`,
		r.URL.Query().Get("state")).Scan(&stored.Nonce, &stored.CodeVerifier, &stored.CreatedAt)
	if err != nil {
		deny("state_replay_or_unknown")
		return
	}
	if err := oidc.ValidateState(&oidc.AuthorizationRequest{
		State: r.URL.Query().Get("state"), Nonce: stored.Nonce, CodeVerifier: stored.CodeVerifier, CreatedAt: stored.CreatedAt,
	}, r.URL.Query().Get("state"), s.now()); err != nil {
		deny("state_expired")
		return
	}

	// 2. Identity resolution: the Feishu profile exchanges the code and binds
	// identity via the server-side user_info call; standard OIDC exchanges
	// with PKCE and validates the ID token (JWKS signature, iss/aud/nonce).
	var result oidc.TokenResult
	if s.OIDCFeishu != nil {
		result, err = s.OIDCFeishu.ExchangeIdentity(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			log.Printf("gateway: feishu identity exchange failed: %v", err)
			deny("identity_exchange_failed")
			return
		}
		if result.Diagnostics != "" {
			log.Printf("gateway: feishu identity %s (subject=%s)", result.Diagnostics, result.Subject)
		}
		result.Expiry = s.now().Add(oidc.SessionTTL)
	} else {
		rawIDToken, xerr := s.OIDC.ExchangeCode(r.Context(), oidc.ExchangeInput{
			Code:          r.URL.Query().Get("code"),
			ExpectedState: r.URL.Query().Get("state"),
			Stored: &oidc.AuthorizationRequest{
				Nonce: stored.Nonce, CodeVerifier: stored.CodeVerifier, CreatedAt: stored.CreatedAt,
			},
			ReceivedState: r.URL.Query().Get("state"),
		})
		if xerr != nil {
			log.Printf("gateway: oidc token exchange failed: %v", xerr)
			deny("token_exchange_failed")
			return
		}
		result, xerr = oidc.ValidateIDToken(rawIDToken, stored.Nonce, *s.OIDC, s.JWKSFetch)
		if xerr != nil {
			log.Printf("gateway: oidc id token rejected (%v): %v", errors.Is(xerr, oidc.ErrBadIDToken), xerr)
			deny("id_token_rejected")
			return
		}
	}

	// 4. Claim mapping through the canonical boundary. IdP group names map
	// through the explicit binding table to workspaces AND the roles those
	// groups grant; sync is additive (out-of-band grants survive re-login)
	// and unmapped workspaces are revoked.
	bindings, err := s.OIDCStore.ResolveGroupBindings(r.Context(), result.Groups)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "workspace mapping failed"))
		return
	}
	if len(bindings) == 0 {
		deny("no_workspace_membership")
		return
	}
	user, err := s.Store.UpsertUser(r.Context(), store.IDClaims{
		Subject: result.Subject, Email: result.Email, DisplayName: result.DisplayName,
		OrganizationID: "org_default", DepartmentID: "dept_platform",
	})
	if err != nil {
		log.Printf("gateway: oidc user sync failed: %v", err)
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "user sync failed"))
		return
	}
	if err := s.OIDCStore.SyncGroupBindings(r.Context(), user.ID, bindings); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "membership sync failed"))
		return
	}
	workspaceIDs := make([]string, 0, len(bindings))
	for _, b := range bindings {
		workspaceIDs = append(workspaceIDs, b.WorkspaceID)
	}

	// 5. Gateway session: opaque id in a HttpOnly cookie; identity snapshot
	// lives server-side. No OIDC/identity token reaches the browser.
	sessionID := randomSessionID()
	expires := s.now().Add(oidc.SessionTTL)
	if _, err := s.OIDCStore.Pool().Exec(r.Context(), `
		INSERT INTO portal_sessions (session_id, user_id, email, display_name, idp_subject, workspaces, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sessionID, user.ID, user.Email, user.DisplayName, result.Subject, workspaceIDs, expires); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "session persist failed"))
		return
	}

	ev := audit.NewEvent(audit.EventAuthSucceeded, audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: user.ID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Detail = map[string]any{"credential_type": "oidc"}
	if err := audit.RequireWrite(r.Context(), s.OIDCStore, ev); err != nil {
		log.Printf("gateway: SECURITY AUDIT delivery failure: %v", err)
	}

	http.SetCookie(w, oidc.SessionCookie(sessionID, s.now(), s.OIDCSecure))
	http.Redirect(w, r, "/portal/overview", http.StatusFound)
}

// logoutOIDC implements POST /enterprise/v1/auth/oidc/logout: revokes the
// server session and clears the cookie.
func (s *Server) logoutOIDC(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(oidc.SessionCookieName); err == nil && c.Value != "" {
		if _, err := s.OIDCStore.Pool().Exec(r.Context(),
			`UPDATE portal_sessions SET revoked_at = now() WHERE session_id = $1 AND revoked_at IS NULL`, c.Value); err != nil {
			log.Printf("gateway: session revoke failed: %v", err)
		}
	}
	http.SetCookie(w, oidc.ClearedCookie(s.now(), s.OIDC != nil && s.OIDCSecure))
	w.WriteHeader(http.StatusNoContent)
}

// requirePortalSession authenticates a Portal request from the session
// cookie. Removed membership / revoked or expired sessions fail closed.
func (s *Server) requirePortalSession(w http.ResponseWriter, r *http.Request) (identity.Session, bool) {
	c, err := r.Cookie(oidc.SessionCookieName)
	if err != nil || c.Value == "" {
		writeProblem(w, r, errs.New(errs.Unauthenticated, "no portal session"))
		return identity.Session{}, false
	}
	var (
		userID      string
		displayName string
		workspaces  []string
	)
	err = s.OIDCStore.Pool().QueryRow(r.Context(), `
		SELECT user_id, display_name, workspaces FROM portal_sessions
		WHERE session_id = $1 AND revoked_at IS NULL AND expires_at > $2`,
		c.Value, s.now()).Scan(&userID, &displayName, &workspaces)
	if err != nil {
		writeProblem(w, r, errs.New(errs.Unauthenticated, "portal session is invalid or expired"))
		return identity.Session{}, false
	}
	// Membership is re-validated server-side right now: a user whose
	// membership was removed after login loses access immediately.
	ws := make([]identity.WorkspaceContext, 0, len(workspaces))
	selected := r.Header.Get(identity.HeaderWorkspace)
	for _, id := range workspaces {
		m, err := s.Store.IsMember(r.Context(), userID, id)
		if err != nil {
			writeProblem(w, r, errs.New(errs.Internal, "membership validation failed"))
			return identity.Session{}, false
		}
		if !m.Member {
			continue // deprovisioned: excluded from the session's authority
		}
		ws = append(ws, identity.WorkspaceContext{WorkspaceID: id, Source: identity.WorkspaceSourceMembership, Roles: m.Roles})
	}
	if len(ws) == 0 {
		// Deprovisioned after login: observable, audited, denied.
		ev := audit.NewEvent(audit.EventAuthFailed, audit.OutcomeDenied)
		ev.RequestID = requestIDOf(r)
		ev.TraceID = traceIDOf(r)
		ev.Detail = map[string]any{"reason": "membership_removed", "credential_type": "oidc"}
		if err := audit.RequireWrite(r.Context(), s.OIDCStore, ev); err != nil {
			log.Printf("gateway: SECURITY AUDIT delivery failure: %v", err)
		}
		writeProblem(w, r, errs.WorkspaceFailClosed("session has no remaining workspace membership"))
		return identity.Session{}, false
	}
	chosen := ws[0]
	if selected != "" {
		for _, w2 := range ws {
			if w2.WorkspaceID == selected {
				chosen = w2
				break
			}
		}
		if chosen.WorkspaceID != selected {
			writeProblem(w, r, errs.WorkspaceFailClosed("workspace is not part of this session"))
			return identity.Session{}, false
		}
	}
	return identity.Session{
		Principal: identity.Principal{UserID: userID, OrganizationID: "org_default"},
		Workspace: chosen,
	}, true
}

// handleOIDCProfile returns the session's Principal/workspace context for
// the Portal (HttpOnly-cookie based, replaces /enterprise/v1/profile when
// OIDC is enabled).
func (s *Server) handleOIDCProfile(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePortalSession(w, r)
	if !ok {
		return
	}
	user, err := s.Store.GetUser(r.Context(), session.Principal.UserID)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "user lookup failed"))
		return
	}
	memberships, err := s.Store.ListMemberships(r.Context(), user.ID)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "membership lookup failed"))
		return
	}
	wsList := make([]enterpriseWorkspaceView, 0, len(memberships))
	defaultWS := ""
	for i, m := range memberships {
		wsList = append(wsList, enterpriseWorkspaceView{ID: m.WorkspaceID, Name: m.Name, Default: m.Default, Roles: m.Roles})
		if m.Default || (defaultWS == "" && i == 0) {
			defaultWS = m.WorkspaceID
		}
	}
	// Control-plane session JWT for the Portal's API calls (memory-only in
	// the browser; the HttpOnly cookie only bootstraps this issuance and is
	// re-validated server-side on every bootstrap).
	sessionToken, err := s.Signer.Sign(token.IssueInput{
		Session:  identity.Session{Principal: identity.Principal{UserID: user.ID, OrganizationID: user.OrganizationID, DepartmentID: user.DepartmentID}},
		Audience: token.AudienceAgentGateway,
		TTL:      oidc.SessionTTL,
		Now:      s.now(),
	})
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "session issuance failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id": user.ID, "display_name": user.DisplayName, "email": user.Email,
			"organization_id": user.OrganizationID, "department_id": user.DepartmentID,
		},
		"workspaces":        wsList,
		"default_workspace": defaultWS,
		"session_token":     sessionToken,
	})
}

type enterpriseWorkspaceView struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Default bool     `json:"default,omitempty"`
	Roles   []string `json:"roles,omitempty"`
}

func randomSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
