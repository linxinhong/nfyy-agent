package main

// Device authorization flow (RFC 8628 shape) and versioned CLI bootstrap
// (TASK-015, ADR-0014). Refresh credentials are stored hashed; rotation is
// one-time; the device authorization is consumed on first successful token
// issuance; approval/denial are explicit control-plane actions.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"enterprise-agent-platform/packages/agentdist"
	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/oidc"
	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/store"
	"enterprise-agent-platform/packages/token"
)

// Device-flow constants (RFC 8628 shape, gateway-managed grants).
const (
	deviceCodeTTL     = 10 * time.Minute
	devicePollMin     = 3 * time.Second
	devicePollSlowAt  = 3 // polls before slow_down kicks in
	refreshTTL        = 30 * 24 * time.Hour
	userCodeCharset   = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	GrantDeviceCode   = "urn:ietf:params:oauth:grant-type:device_code"
	GrantRefreshToken = "refresh_token"
)

// DeviceCodeResponse is the RFC 8628 authorization response.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func randomUserCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = userCodeCharset[int(c)%len(userCodeCharset)]
	}
	return string(out), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// handleDeviceCode implements POST /enterprise/v1/auth/device/code.
func (s *Server) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	dc, err := randomUserCode()
	if err != nil {
		writeProblem(w, r, errs.New(errs.Internal, "random generation failed"))
		return
	}
	userCode, err := randomUserCode()
	if err != nil {
		writeProblem(w, r, errs.New(errs.Internal, "random generation failed"))
		return
	}
	deviceCode := "dc_" + dc
	userCode = strings.ToLower(userCode)
	if _, err := s.OIDCStore.Pool().Exec(r.Context(), `
		INSERT INTO device_authorizations (device_code, user_code, expires_at)
		VALUES ($1, $2, $3)`, deviceCode, userCode, s.now().Add(deviceCodeTTL)); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "device flow persist failed"))
		return
	}
	writeJSON(w, http.StatusOK, DeviceCodeResponse{
		DeviceCode:      deviceCode,
		UserCode:        userCode,
		VerificationURI: "/portal/device",
		ExpiresIn:       int(deviceCodeTTL / time.Second),
		Interval:        int(devicePollMin / time.Second),
	})
}

// deviceSessionUser authenticates an approval request via a control-plane
// session JWT (aud=agent-gateway); the approving user IS the Principal.
func (s *Server) deviceSessionUser(r *http.Request) (string, error) {
	raw, ok := bearerToken(r)
	if !ok {
		return "", errs.New(errs.Unauthenticated, "approval requires a login session")
	}
	claims, err := s.Verifier.Verify(raw, token.AudienceAgentGateway)
	if err != nil {
		return "", errs.New(errs.Unauthenticated, "invalid approval session")
	}
	return claims.Subject, nil
}

// handleDeviceApprove implements POST /enterprise/v1/auth/device/approve.
func (s *Server) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	s.decideDevice(w, r, "approved")
}

// handleDeviceDeny implements POST /enterprise/v1/auth/device/deny.
func (s *Server) handleDeviceDeny(w http.ResponseWriter, r *http.Request) {
	s.decideDevice(w, r, "denied")
}

func (s *Server) decideDevice(w http.ResponseWriter, r *http.Request, decision string) {
	userID, err := s.deviceSessionUser(r)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	var body struct {
		UserCode string `json:"user_code"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	if body.UserCode == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "user_code is required"))
		return
	}
	memberships, err := s.Store.ListMemberships(r.Context(), userID)
	if err != nil || len(memberships) == 0 {
		writeProblem(w, r, errs.WorkspaceFailClosed("approving user has no workspace membership"))
		return
	}
	var deviceCode string
	err = s.OIDCStore.Pool().QueryRow(r.Context(), `
		UPDATE device_authorizations
		SET status = $2, user_id = $3, workspace_id = $4, decided_at = now(), poll_count = 0
		WHERE user_code = $1 AND status = 'pending' AND expires_at > now()
		RETURNING device_code`,
		strings.ToLower(body.UserCode), decision, userID, memberships[0].WorkspaceID).
		Scan(&deviceCode)
	if err != nil {
		writeProblem(w, r, errs.New(errs.NotFound, "no pending device authorization with this code"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": decision})
}

// handleDeviceToken implements POST /enterprise/v1/auth/device/token: the
// polling endpoint (device_code grant) and the refresh endpoint
// (refresh_token grant, one-time rotation).
func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GrantType    string `json:"grant_type"`
		DeviceCode   string `json:"device_code"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	switch body.GrantType {
	case GrantDeviceCode:
		s.deviceTokenPoll(w, r, body.DeviceCode)
	case GrantRefreshToken:
		s.deviceTokenRefresh(w, r, body.RefreshToken)
	default:
		oauthError(w, "unsupported_grant_type", http.StatusBadRequest)
	}
}

func oauthError(w http.ResponseWriter, code string, status int) {
	writeJSON(w, status, map[string]any{"error": code})
}

func (s *Server) deviceTokenPoll(w http.ResponseWriter, r *http.Request, deviceCode string) {
	if deviceCode == "" {
		oauthError(w, "invalid_request", http.StatusBadRequest)
		return
	}
	var (
		status    string
		userID    *string
		wsID      *string
		pollCount int
		expiresAt time.Time
	)
	err := s.OIDCStore.Pool().QueryRow(r.Context(), `
		UPDATE device_authorizations SET poll_count = poll_count + 1
		WHERE device_code = $1
		RETURNING status, user_id, workspace_id, poll_count, expires_at`,
		deviceCode).Scan(&status, &userID, &wsID, &pollCount, &expiresAt)
	if err != nil {
		oauthError(w, "expired_token", http.StatusBadRequest)
		return
	}
	if s.now().After(expiresAt) {
		oauthError(w, "expired_token", http.StatusBadRequest)
		return
	}
	if pollCount > devicePollSlowAt {
		oauthError(w, "slow_down", http.StatusTooManyRequests)
		return
	}
	switch status {
	case "pending":
		oauthError(w, "authorization_pending", http.StatusBadRequest)
		return
	case "denied":
		s.auditAuthFailedDetail(r, userID, "device_flow", map[string]any{"reason": "access_denied"})
		oauthError(w, "access_denied", http.StatusForbidden)
		return
	}

	// Approved: mint refresh credential + control-plane access session.
	// The authorization is single-use — consumed on first successful token
	// issuance so a leaked approved code cannot be re-polled.
	user := derefOrEmpty(userID)
	workspace := derefOrEmpty(wsID)
	if _, err := s.OIDCStore.Pool().Exec(r.Context(),
		`DELETE FROM device_authorizations WHERE device_code = $1`, deviceCode); err != nil {
		log.Printf("gateway: device authorization consume failed: %v", err)
		oauthError(w, "server_error", http.StatusInternalServerError)
		return
	}
	cred := s.mintDeviceCredential(r, user)
	if cred == "" {
		oauthError(w, "server_error", http.StatusInternalServerError)
		return
	}
	access, err := s.Signer.Sign(tokenIssueFor(user, workspace))
	if err != nil {
		oauthError(w, "server_error", http.StatusInternalServerError)
		return
	}
	s.auditDevice(r, user, "device.login", audit.OutcomeAllowed)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":       access,
		"token_type":         "Bearer",
		"refresh_token":      cred,
		"expires_in":         int(oidc.SessionTTL / time.Second),
		"refresh_expires_in": int(refreshTTL / time.Second),
	})
}

func (s *Server) deviceTokenRefresh(w http.ResponseWriter, r *http.Request, refreshToken string) {
	if refreshToken == "" {
		oauthError(w, "invalid_request", http.StatusBadRequest)
		return
	}
	var (
		id        int64
		userID    string
		status    string
		expiresAt time.Time
	)
	err := s.OIDCStore.Pool().QueryRow(r.Context(), `
		SELECT id, user_id, status, expires_at FROM device_credentials
		WHERE refresh_hash = $1`, hashToken(refreshToken)).
		Scan(&id, &userID, &status, &expiresAt)
	if err != nil {
		s.auditAuthFailedDetail(r, nil, "device_flow", map[string]any{"reason": "unknown_refresh_token"})
		oauthError(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	// Replay of a rotated/revoked credential fails closed.
	if status != "active" || s.now().After(expiresAt) {
		s.auditAuthFailedDetail(r, &userID, "device_flow", map[string]any{"reason": "refresh_replay_or_revoked"})
		oauthError(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	memberships, err := s.Store.ListMemberships(r.Context(), userID)
	if err != nil || len(memberships) == 0 {
		s.auditAuthFailedDetail(r, &userID, "device_flow", map[string]any{"reason": "membership_removed"})
		oauthError(w, "invalid_grant", http.StatusForbidden)
		return
	}
	newCred := s.mintDeviceCredential(r, userID)
	if newCred == "" {
		oauthError(w, "server_error", http.StatusInternalServerError)
		return
	}
	if _, err := s.OIDCStore.Pool().Exec(r.Context(),
		`UPDATE device_credentials SET status = 'rotated' WHERE id = $1`, id); err != nil {
		log.Printf("gateway: rotate mark failed: %v", err)
	}
	access, err := s.Signer.Sign(tokenIssueFor(userID, memberships[0].WorkspaceID))
	if err != nil {
		oauthError(w, "server_error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":       access,
		"token_type":         "Bearer",
		"refresh_token":      newCred,
		"expires_in":         int(oidc.SessionTTL / time.Second),
		"refresh_expires_in": int(refreshTTL / time.Second),
	})
}

// mintDeviceCredential stores the hashed refresh credential; the raw value
// is returned once to the client.
func (s *Server) mintDeviceCredential(r *http.Request, userID string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	raw := base64.RawURLEncoding.EncodeToString(b)
	if _, err := s.OIDCStore.Pool().Exec(r.Context(), `
		INSERT INTO device_credentials (refresh_hash, user_id, expires_at)
		VALUES ($1, $2, $3)`, hashToken(raw), userID, s.now().Add(refreshTTL)); err != nil {
		log.Printf("gateway: device credential persist failed: %v", err)
		return ""
	}
	return raw
}

func (s *Server) auditDevice(r *http.Request, userID, eventType string, outcome audit.Outcome) {
	ev := audit.NewEvent(audit.EventType(eventType), outcome)
	ev.Actor = &identity.Principal{UserID: userID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Detail = map[string]any{"credential_type": "device_flow"}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		log.Printf("gateway: SECURITY AUDIT delivery failure: %v", err)
	}
}

// handleDeviceLogout implements POST /enterprise/v1/auth/device/logout.
func (s *Server) handleDeviceLogout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	if body.RefreshToken == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "refresh_token is required"))
		return
	}
	var userID string
	_ = s.OIDCStore.Pool().QueryRow(r.Context(),
		`SELECT user_id FROM device_credentials WHERE refresh_hash = $1`,
		hashToken(body.RefreshToken)).Scan(&userID)
	if _, err := s.OIDCStore.Pool().Exec(r.Context(),
		`UPDATE device_credentials SET status = 'revoked' WHERE refresh_hash = $1 AND status = 'active'`,
		hashToken(body.RefreshToken)); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "logout failed"))
		return
	}
	if userID != "" {
		s.auditDevice(r, userID, "device.logout", audit.OutcomeAllowed)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCLIBootstrap implements GET /enterprise/v1/cli/bootstrap: the
// versioned Workspace bootstrap, authenticated by device access session or
// agk key. ETag = manifest content hash; If-None-Match -> 304.
func (s *Server) handleCLIBootstrap(w http.ResponseWriter, r *http.Request) {
	var userID, workspace string
	raw, hasBearer := bearerToken(r)
	switch {
	case hasBearer && strings.HasPrefix(raw, "agk_"):
		auth, ok := s.requireAPIKey(w, r)
		if !ok {
			return
		}
		session, err := s.selectWorkspace(r, auth)
		if err != nil {
			writeProblem(w, r, err)
			return
		}
		userID, workspace = session.Principal.UserID, session.Workspace.WorkspaceID
	case hasBearer:
		claims, err := s.Verifier.Verify(raw, token.AudienceAgentGateway)
		if err != nil {
			writeProblem(w, r, errs.New(errs.Unauthenticated, "invalid device session"))
			return
		}
		memberships, err := s.Store.ListMemberships(r.Context(), claims.Subject)
		if err != nil || len(memberships) == 0 {
			writeProblem(w, r, errs.WorkspaceFailClosed("no workspace membership"))
			return
		}
		userID, workspace = claims.Subject, memberships[0].WorkspaceID
	default:
		writeProblem(w, r, errs.New(errs.Unauthenticated, "device or api key credential required"))
		return
	}

	user, err := s.Store.GetUser(r.Context(), userID)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "user lookup failed"))
		return
	}
	memberships, err := s.Store.ListMemberships(r.Context(), userID)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "membership lookup failed"))
		return
	}
	agentRows, err := s.OIDCStore.Pool().Query(r.Context(), `
		SELECT client, MAX(minimum_version) FROM agent_releases
		WHERE workspace_id = $1 AND status = 'released'
		GROUP BY client`, workspace)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "agent policy lookup failed"))
		return
	}
	defer agentRows.Close()
	agents := []map[string]any{}
	for agentRows.Next() {
		var client, minimum string
		if err := agentRows.Scan(&client, &minimum); err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "agent policy scan failed"))
			return
		}
		agents = append(agents, map[string]any{"client": client, "minimum_version": minimum})
	}
	manifest := map[string]any{
		"manifest_version": 1,
		"user": map[string]any{
			"id": user.ID, "display_name": user.DisplayName, "email": user.Email,
		},
		"workspace":  workspace,
		"workspaces": membershipsToViews(memberships),
		"agents":     agents,
		"endpoints": map[string]string{
			"llm":        "/v1",
			"mcp":        "/mcp/{server}",
			"knowledge":  "/enterprise/v1/knowledge",
			"enterprise": "/enterprise/v1",
		},
	}
	encoded, _ := json.Marshal(manifest)
	etag := fmt.Sprintf(`"%x"`, sha256Sum(encoded))
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, manifest)
}

func membershipsToViews(ms []store.Membership) []map[string]any {
	out := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		out = append(out, map[string]any{"id": m.WorkspaceID, "name": m.Name, "roles": m.Roles})
	}
	return out
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// tokenIssueFor builds the IssueInput for a device-plane access session.
func tokenIssueFor(userID, workspace string) token.IssueInput {
	return token.IssueInput{
		Session: identity.Session{
			Principal: identity.Principal{UserID: userID, OrganizationID: "org_default"},
			Workspace: identity.WorkspaceContext{WorkspaceID: workspace, Source: identity.WorkspaceSourceMembership},
		},
		Audience: token.AudienceAgentGateway,
		TTL:      oidc.SessionTTL,
		Now:      time.Now(),
	}
}

// sha256Sum returns the hex digest of the manifest bytes (ETag basis).
func sha256Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

// handleAgentList implements GET /enterprise/v1/agents/{client}: releases
// matching the caller's workspace and platform (no payload, no storage
// paths, no credentials).
// clientPlatform validates a caller-declared OS/architecture pair. The
// gateway never falls back to its own platform: agent binaries are
// platform-specific and the server's runtime says nothing about the client.
func validPlatform(goos, goarch string) bool {
	switch goos {
	case "darwin", "windows", "linux":
	default:
		return false
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return false
	}
	return true
}

func clientPlatform(r *http.Request) (goos, goarch string, err error) {
	goos, goarch = r.URL.Query().Get("os"), r.URL.Query().Get("arch")
	if !validPlatform(goos, goarch) {
		return "", "", errs.New(errs.InvalidArgument, "os must be one of darwin, windows, linux and arch one of amd64, arm64")
	}
	return goos, goarch, nil
}

func (s *Server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	// Read-only catalog: the Portal browser holds a session JWT, CLI/agents
	// hold agk keys — both are legitimate catalog readers.
	auth, ok := s.requireCatalogAuth(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	client := r.PathValue("client")
	goos, goarch, err := clientPlatform(r)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	rows, err := s.OIDCStore.Pool().Query(r.Context(), `
		SELECT client, version, os, arch, minimum_version, notes, digest, signed_by
		FROM agent_releases
		WHERE client = $1 AND workspace_id = $2 AND status = 'released'
		  AND os = $3 AND arch = $4
		ORDER BY version DESC`,
		client, session.Workspace.WorkspaceID, goos, goarch)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "agent release listing failed"))
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var rel struct {
			Client, Version, OS, Arch, MinimumVersion, Notes, Digest, SignedBy string
		}
		if err := rows.Scan(&rel.Client, &rel.Version, &rel.OS, &rel.Arch, &rel.MinimumVersion, &rel.Notes, &rel.Digest, &rel.SignedBy); err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "agent release scan failed"))
			return
		}
		out = append(out, map[string]any{
			"client": rel.Client, "version": rel.Version, "os": rel.OS, "arch": rel.Arch,
			"minimum_version": rel.MinimumVersion, "notes": rel.Notes, "digest": rel.Digest,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": out})
}

// handleAgentTicket implements POST /enterprise/v1/agents/{client}/ticket:
// issues a short-lived same-domain download ticket after re-checking
// workspace assignment, release status and version policy.
func (s *Server) handleAgentTicket(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	client := r.PathValue("client")
	var body struct {
		Version string `json:"version"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	q := r.URL.Query()
	if body.OS == "" {
		body.OS = q.Get("os")
	}
	if body.Arch == "" {
		body.Arch = q.Get("arch")
	}
	if !validPlatform(body.OS, body.Arch) {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "os and arch of the installing client are required (darwin|windows|linux, amd64|arm64)"))
		return
	}
	goos, goarch := body.OS, body.Arch
	var (
		releaseID string
		minVer    string
	)
	err = s.OIDCStore.Pool().QueryRow(r.Context(), `
		SELECT id, minimum_version FROM agent_releases
		WHERE client = $1 AND version = $2 AND workspace_id = $3 AND os = $4 AND arch = $5 AND status = 'released'`,
		client, body.Version, session.Workspace.WorkspaceID, goos, goarch).Scan(&releaseID, &minVer)
	if err != nil {
		writeProblem(w, r, errs.New(errs.NotFound, "no released agent version matching this workspace/platform"))
		return
	}
	if !agentdist.VersionMeet(body.Version, minVer) {
		s.auditAgentDenied(r, session.Principal.UserID, client, body.Version, minVer)
		writeProblem(w, r, errs.New(errs.PermissionDenied,
			fmt.Sprintf("version %s is below the workspace minimum %s — upgrade required", body.Version, minVer)))
		return
	}
	ticket, err := agentdist.NewTicket()
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if _, err := s.OIDCStore.Pool().Exec(r.Context(), `
		INSERT INTO download_tickets (ticket, release_id, user_id, workspace_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		ticket, releaseID, session.Principal.UserID, session.Workspace.WorkspaceID,
		s.now().Add(2*time.Minute)); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "ticket persist failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     ticket,
		"expires_in": 120,
		"redeem":     "/enterprise/v1/agents/" + client + "/download",
	})
}

// handleAgentDownload implements POST /enterprise/v1/agents/{client}/download:
// redeems the ticket (single redemption, re-checking workspace and release
// status) and streams the release bytes with digest + signature headers.
func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	var (
		releaseID string
		wsID      string
	)
	err = s.OIDCStore.Pool().QueryRow(r.Context(), `
		UPDATE download_tickets SET redeemed_at = now()
		WHERE ticket = $1 AND user_id = $2 AND redeemed_at IS NULL AND expires_at > now()
		RETURNING release_id, workspace_id`,
		body.Ticket, session.Principal.UserID).Scan(&releaseID, &wsID)
	if err != nil {
		writeProblem(w, r, errs.New(errs.PermissionDenied, "ticket is unknown, expired or already redeemed"))
		return
	}
	var (
		payload   []byte
		digest    string
		signature string
	)
	err = s.OIDCStore.Pool().QueryRow(r.Context(), `
		SELECT payload, digest, signature FROM agent_releases
		WHERE id = $1 AND workspace_id = $2 AND status = 'released'`,
		releaseID, wsID).Scan(&payload, &digest, &signature)
	if err != nil {
		writeProblem(w, r, errs.New(errs.NotFound, "release is no longer available"))
		return
	}
	w.Header().Set("X-Agent-Digest", "sha256-"+digest)
	sigEnvelope := skillreg.Signature{Kid: "pkg-agent", Alg: "EdDSA", Digest: digest, Signature: signature}
	if sigJSON, err := json.Marshal(sigEnvelope); err == nil {
		w.Header().Set("X-Agent-Signature", string(sigJSON))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(payload)
}

// auditAgentDenied records the reliable-path security audit for a
// minimum-version ticket denial (TASK-019).
func (s *Server) auditAgentDenied(r *http.Request, userID, client, version, minimum string) {
	ev := audit.NewEvent("agent.version_denied", audit.OutcomeDenied)
	ev.Actor = &identity.Principal{UserID: userID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Detail = map[string]any{"client": client, "version": version, "minimum_version": minimum}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		log.Printf("gateway: SECURITY AUDIT delivery failure: %v", err)
	}
}
