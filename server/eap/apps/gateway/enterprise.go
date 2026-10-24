package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"enterprise-agent-platform/packages/apikey"
	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/protocol-enterprise"
	"enterprise-agent-platform/packages/store"
	"enterprise-agent-platform/packages/token"
)

const maxBodyBytes = 1 << 20 // 1 MiB request cap for JSON endpoints

// decodeJSONBody parses a JSON request body with a size cap.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errs.New(errs.InvalidArgument, "request body is required")
		}
		return errs.New(errs.InvalidArgument, "request body must be valid JSON")
	}
	return nil
}

// handleDevLogin is the development OIDC stand-in (ADR-0002): the request
// plays the role of a verified id_token, so the claims it carries are treated
// as IdP-asserted. It is disabled unless EAP_DEV_MODE=1; production
// authentication binds to the OIDC adapter boundary, not this endpoint.
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	if !s.DevMode {
		writeProblem(w, r, errs.New(errs.NotFound, "dev login is not available"))
		return
	}
	var body struct {
		Email       string   `json:"email"`
		DisplayName string   `json:"display_name,omitempty"`
		Workspaces  []string `json:"workspaces,omitempty"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	body.Email = strings.TrimSpace(body.Email)
	if body.Email == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "email is required"))
		return
	}

	claims := store.IDClaims{
		Subject:        "dev:" + body.Email,
		Email:          body.Email,
		DisplayName:    body.DisplayName,
		OrganizationID: "org_default",
		DepartmentID:   "dept_platform",
	}
	user, err := s.Store.UpsertUser(r.Context(), claims)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "user sync failed"))
		return
	}
	if len(body.Workspaces) > 0 {
		if err := s.Store.SyncMemberships(r.Context(), user.ID, body.Workspaces, []string{"developer"}); err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "membership sync failed"))
			return
		}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"session_token": session,
		"token_type":    enterprise.TokenType,
		"expires_in":    int64(token.MaxTTL / time.Second),
	})
}

// requireSession resolves a control-plane session credential.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (authenticated, bool) {
	auth, err := s.authenticate(r)
	if err != nil || auth.Key != nil {
		if err == nil {
			err = errs.New(errs.Unauthenticated, "this endpoint requires a login session")
		}
		writeProblem(w, r, err)
		return authenticated{}, false
	}
	return auth, true
}

// requireCatalogAuth resolves a read-only catalog caller: an employee API
// key (CLI / agents) or the control-plane session JWT (Portal browser).
// Catalogs are read-only and workspace-scoped server-side either way.
func (s *Server) requireCatalogAuth(w http.ResponseWriter, r *http.Request) (authenticated, bool) {
	raw, hasBearer := bearerToken(r)
	if hasBearer && !strings.HasPrefix(raw, apikey.PublicPrefix) {
		if auth, err := s.authenticateSession(r, raw); err == nil {
			return auth, true
		}
		// Fall through: maybe an agk key with a non-public prefix or an
		// invalid session — authenticate below reports the precise error.
	}
	return s.requireAPIKey(w, r)
}

// requireAPIKey resolves an employee API key credential.
func (s *Server) requireAPIKey(w http.ResponseWriter, r *http.Request) (authenticated, bool) {
	auth, err := s.authenticate(r)
	if err != nil || auth.Key == nil {
		if err == nil {
			err = errs.New(errs.Unauthenticated, "this endpoint requires an employee API key")
		}
		writeProblem(w, r, err)
		return authenticated{}, false
	}
	return auth, true
}

// handleCreateKey implements POST /enterprise/v1/keys. The full key value is
// returned exactly once; only its hash is stored.
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	var body enterprise.APIKeyCreateRequest
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "name is required"))
		return
	}
	env := body.Environment
	if env == "" {
		env = enterprise.KeyEnvironmentLive
	}
	if env != enterprise.KeyEnvironmentLive && env != enterprise.KeyEnvironmentTest {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "environment must be live or test"))
		return
	}
	if body.ExpiresAt != nil && !body.ExpiresAt.After(s.now()) {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "expires_at must be in the future"))
		return
	}
	// Optional workspace binding (ADR-0024). Authority is established
	// server-side: only a workspace the creator is a member of can be bound,
	// and resolver failures deny (fail closed).
	workspace := strings.TrimSpace(body.Workspace)
	if workspace != "" {
		m, err := s.Store.IsMember(r.Context(), auth.Principal.UserID, workspace)
		if err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.WorkspaceForbidden, "workspace membership could not be established (fail closed)"))
			return
		}
		if !m.Member {
			writeProblem(w, r, errs.New(errs.WorkspaceForbidden, "principal is not a member of workspace "+workspace))
			return
		}
	}

	generated, err := apikey.Generate(env)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "key generation failed"))
		return
	}

	ev := audit.NewEvent(audit.EventAuthKeyCreated, audit.OutcomeAllowed)
	ev.Actor = &auth.Principal
	ev.Detail = map[string]any{"workspace_bound": workspace}
	key, err := s.Store.CreateAPIKey(r.Context(), auth.Principal.UserID, generated.Hash, generated.Prefix, body.Name, workspace, body.ExpiresAt, ev)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "key creation failed"))
		return
	}
	writeJSON(w, http.StatusCreated, enterprise.APIKeyCreated{
		KeyID:     key.KeyID,
		Key:       generated.Full,
		Prefix:    key.Prefix,
		Name:      key.Name,
		Workspace: workspace,
		CreatedAt: key.CreatedAt,
		ExpiresAt: key.ExpiresAt,
	})
}

// handleListKeys implements GET /enterprise/v1/keys (metadata only).
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	keys, err := s.Store.ListAPIKeys(r.Context(), auth.Principal.UserID)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "key listing failed"))
		return
	}
	views := make([]enterprise.APIKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, keyView(k))
	}
	writeJSON(w, http.StatusOK, views)
}

// handleRevokeKey implements DELETE /enterprise/v1/keys/{keyID} (owner only).
func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	keyID := r.PathValue("keyID")
	if keyID == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "key id is required"))
		return
	}
	ev := audit.NewEvent(audit.EventAuthKeyRevoked, audit.OutcomeAllowed)
	ev.Actor = &auth.Principal
	revoked, err := s.Store.RevokeAPIKey(r.Context(), auth.Principal.UserID, keyID, ev)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "key revocation failed"))
		return
	}
	if !revoked {
		writeProblem(w, r, errs.New(errs.NotFound, "no live key with this id"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMe implements GET /enterprise/v1/me for the employee API key surface.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	s.writeMe(w, r, auth.Principal.UserID)
}

// handleProfile implements GET /enterprise/v1/profile for the Portal session
// surface (TASK-005). Same payload as /me, but authenticated by the
// control-plane session instead of an employee API key.
func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	s.writeMe(w, r, auth.Principal.UserID)
}

func (s *Server) writeMe(w http.ResponseWriter, r *http.Request, userID string) {
	user, err := s.Store.GetUser(r.Context(), userID)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "user lookup failed"))
		return
	}
	memberships, err := s.Store.ListMemberships(r.Context(), user.ID)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "membership lookup failed"))
		return
	}
	ws := make([]enterprise.Workspace, 0, len(memberships))
	defaultWS := ""
	for i, m := range memberships {
		ws = append(ws, enterprise.Workspace{ID: m.WorkspaceID, Name: m.Name, Default: m.Default, Roles: m.Roles})
		if m.Default || (defaultWS == "" && i == 0) {
			defaultWS = m.WorkspaceID
		}
	}
	writeJSON(w, http.StatusOK, enterprise.MeResponse{
		User: enterprise.User{
			ID:             user.ID,
			DisplayName:    user.DisplayName,
			Email:          user.Email,
			OrganizationID: user.OrganizationID,
			DepartmentID:   user.DepartmentID,
		},
		Workspaces:       ws,
		DefaultWorkspace: defaultWS,
	})
}

// handleExchange implements POST /enterprise/v1/token/exchange: employee API
// key + server-validated workspace in, audience-scoped internal JWT out.
func (s *Server) handleExchange(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	var body enterprise.TokenExchangeRequest
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	aud := token.Audience(strings.TrimSpace(body.Audience))
	if !aud.Known() {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "audience is not a registered internal audience"))
		return
	}
	if aud == token.AudienceAgentGateway {
		writeProblem(w, r, errs.New(errs.PermissionDenied, "exchange never mints agent-gateway tokens"))
		return
	}

	// Workspace authority: header selects (falling back to an API key's
	// server-side binding, ADR-0024), membership decides. Resolver failure
	// denies (fail closed).
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		ws, origin := workspaceSelector(r, auth)
		denied := audit.NewEvent(audit.EventAuthFailed, audit.OutcomeDenied)
		denied.Actor = &auth.Principal
		denied.RequestID = requestIDOf(r)
		denied.TraceID = traceIDOf(r)
		denied.Detail = map[string]any{
			"reason":           "workspace_denied",
			"workspace":        ws,
			"workspace_origin": origin,
			"audience":         string(aud),
		}
		if auditErr := audit.RequireWrite(r.Context(), s.Store, denied); auditErr != nil {
			log.Printf("gateway: SECURITY AUDIT delivery failure: %v", auditErr)
		}
		writeProblem(w, r, err)
		return
	}

	ttl := time.Duration(body.TTLSeconds) * time.Second
	signed, err := s.Signer.Sign(token.IssueInput{Session: session, Audience: aud, TTL: ttl, Now: s.now()})
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "token issuance failed"))
		return
	}
	expiresIn := int64(token.DefaultTTL / time.Second)
	if ttl > 0 {
		expiresIn = int64(min(ttl, token.MaxTTL) / time.Second)
	}

	ev := audit.NewEvent(audit.EventTokenExchanged, audit.OutcomeAllowed)
	ev.Actor = &auth.Principal
	ev.WorkspaceID = session.Workspace.WorkspaceID
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Resource = string(aud)
	ev.Detail = map[string]any{"key_id": auth.Key.KeyID, "jti_audience": string(aud)}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		// Fail closed: no token is handed out without its audit evidence.
		log.Printf("gateway: SECURITY AUDIT delivery failure, exchange denied: %v", err)
		writeProblem(w, r, errs.New(errs.Internal, "token exchange unavailable"))
		return
	}
	writeJSON(w, http.StatusOK, enterprise.TokenExchangeResponse{
		AccessToken: signed,
		TokenType:   enterprise.TokenType,
		ExpiresIn:   expiresIn,
	})
}

// handleJWKS serves the public verification keys.
func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	if len(s.JWKSBody) == 0 {
		writeProblem(w, r, errs.New(errs.Internal, "no signing keys configured"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.JWKSBody)
}

func keyView(k store.APIKey) enterprise.APIKeyView {
	workspace := ""
	if k.WorkspaceID != nil {
		workspace = *k.WorkspaceID
	}
	return enterprise.APIKeyView{
		KeyID:      k.KeyID,
		Prefix:     k.Prefix,
		Name:       k.Name,
		Workspace:  workspace,
		CreatedAt:  k.CreatedAt,
		ExpiresAt:  k.ExpiresAt,
		LastUsedAt: k.LastUsedAt,
		LastIP:     k.LastIP,
		ClientID:   k.ClientID,
		RevokedAt:  k.RevokedAt,
	}
}
