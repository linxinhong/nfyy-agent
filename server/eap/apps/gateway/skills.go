package main

// Skill Registry surface (TASK-016, ADR-0015): publish -> review -> release
// (signed) -> download (workspace-scoped) -> revoke, with role/scope
// enforcement and reliable audits on security-significant transitions.
//
// Roles: publishing requires the publisher to be a workspace member (session
// or agk key); review/release/deprecate/revoke require a control-plane
// session holding skill.review at the skill's scope (packages/skillshare;
// Workspace-wide when sharing is not wired), never on one's own skill.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/rbac"
	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillshare"
	"enterprise-agent-platform/packages/token"
)

// gatewayIdentities is the session projection for skill handlers.
type gatewayIdentities struct {
	userID    string
	workspace string
	roles     []string
	grants    rbac.Grants
}

// requireControlPlane authenticates a control-plane session and resolves its
// permissions from the live membership.
func (s *Server) requireControlPlane(w http.ResponseWriter, r *http.Request) (gatewayIdentities, bool) {
	raw, ok := bearerToken(r)
	if !ok {
		writeProblem(w, r, errs.New(errs.Unauthenticated, "control-plane session required"))
		return gatewayIdentities{}, false
	}
	claims, err := s.Verifier.Verify(raw, token.AudienceAgentGateway)
	if err != nil {
		writeProblem(w, r, errs.New(errs.Unauthenticated, "invalid control-plane session"))
		return gatewayIdentities{}, false
	}
	// 会话可能签发于 Workspace 选择之前（如 dev-login）：服务端解析首个
	// 成员资格作为 scoping，绝不信任客户端声明。
	workspaceID := claims.WorkspaceID
	member, err := s.Store.IsMember(r.Context(), claims.Subject, workspaceID)
	if err != nil || !member.Member {
		// 会话可能签发于 Workspace 选择之前（如 dev-login）：服务端解析
		// 首个成员资格作为 scoping，绝不信任客户端声明。
		memberships, merr := s.Store.ListMemberships(r.Context(), claims.Subject)
		if merr != nil || len(memberships) == 0 {
			writeProblem(w, r, errs.WorkspaceFailClosed("session has no workspace membership"))
			return gatewayIdentities{}, false
		}
		workspaceID = memberships[0].WorkspaceID
		member, err = s.Store.IsMember(r.Context(), claims.Subject, workspaceID)
		if err != nil || !member.Member {
			writeProblem(w, r, errs.WorkspaceFailClosed("session has no workspace membership"))
			return gatewayIdentities{}, false
		}
	}
	grants, err := s.grantsFor(r.Context(), workspaceID, claims.Subject, member.Roles)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "permission lookup failed"))
		return gatewayIdentities{}, false
	}
	return gatewayIdentities{
		userID: claims.Subject, workspace: workspaceID,
		roles: member.Roles, grants: grants,
	}, true
}

// handleSkillPublish implements POST /enterprise/v1/skills/{name}/versions:
// workspace member publishes a pending_review version (server re-validates
// packaging + scans; name@version immutable).
func (s *Server) handleSkillPublish(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireCatalogAuth(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "skill name must be [a-z0-9-]{1,64}"))
		return
	}
	var body struct {
		Version    string                 `json:"version"`
		Files      []skillreg.PackageFile `json:"files"`
		Visibility string                 `json:"visibility"`
		GroupID    string                 `json:"group_id"`
		Protected  bool                   `json:"protected"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	if !validSkillVersion(body.Version) {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "skill version must be semver-like (x.y.z)"))
		return
	}
	if s.Skills != nil {
		s.publishShared(w, r, session, name, body.Version, body.Visibility, body.GroupID, body.Protected, body.Files)
		return
	}
	if body.Visibility != "" && body.Visibility != skillshare.VisibilityOrganization || body.Protected {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "skill sharing is not enabled on this gateway"))
		return
	}
	archive, digest, err := skillreg.BuildArchive(body.Files)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	var id string
	err = s.OIDCStore.Pool().QueryRow(r.Context(), `
		INSERT INTO skill_packages (name, version, workspace_id, digest, payload, published_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`, name, body.Version, session.Workspace.WorkspaceID, digest, archive, session.Principal.UserID).
		Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique") {
			writeProblem(w, r, errs.New(errs.Conflict, "skill version already exists (immutable name@version)"))
			return
		}
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "skill publish failed"))
		return
	}
	if err := s.skillAuditFail(w, r, "skill.published", name, body.Version, audit.OutcomeAllowed,
		map[string]any{"digest": digest, "files": len(body.Files), "workspace": session.Workspace.WorkspaceID}); err != nil {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": name, "version": body.Version,
		"status": skillreg.StatusPendingReview, "digest": digest,
	})
}

// handleSkillReview implements POST .../versions/{version}/review
// (skill.review at the skill's scope): pending_review -> approved.
func (s *Server) handleSkillReview(w http.ResponseWriter, r *http.Request) {
	session, ok := s.skillDecision(w, r)
	if !ok {
		return
	}
	name, version := r.PathValue("name"), r.PathValue("version")
	tag, err := s.OIDCStore.Pool().Exec(r.Context(), `
		UPDATE skill_packages SET status = 'approved', reviewed_by = $3
		WHERE name = $1 AND version = $2 AND status = 'pending_review' AND workspace_id = $4`,
		name, version, session.userID, session.workspace)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "review failed"))
		return
	}
	if tag.RowsAffected() == 0 {
		writeProblem(w, r, errs.New(errs.NotFound, "no pending_review version with this name@version in the workspace"))
		return
	}
	if err := s.skillAuditFail(w, r, "skill.reviewed", name, version, audit.OutcomeAllowed,
		map[string]any{"reviewer": session.userID}); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": skillreg.StatusApproved})
}

// handleSkillRelease implements POST .../versions/{version}/release
// (skill.review at the skill's scope): approved -> released with a platform signature from the
// ACTIVE package key; rotation keeps historical signatures verifiable.
func (s *Server) handleSkillRelease(w http.ResponseWriter, r *http.Request) {
	session, ok := s.skillDecision(w, r)
	if !ok {
		return
	}
	name, version := r.PathValue("name"), r.PathValue("version")
	var (
		id      string
		digest  string
		status  string
		payload []byte
	)
	err := s.OIDCStore.Pool().QueryRow(r.Context(), `
		SELECT id, digest, status, payload FROM skill_packages
		WHERE name = $1 AND version = $2 AND workspace_id = $3`,
		name, version, session.workspace).Scan(&id, &digest, &status, &payload)
	if err != nil {
		writeProblem(w, r, errs.New(errs.NotFound, "no such skill version in the workspace"))
		return
	}
	if status != skillreg.StatusApproved {
		writeProblem(w, r, errs.New(errs.Conflict,
			"release requires status approved (review first); current: "+status))
		return
	}
	key, err := s.activePackageKey(r.Context())
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	sig, err := skillreg.SignRelease(key.kid, key.priv, digest, s.now())
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if _, err := s.OIDCStore.Pool().Exec(r.Context(), `
		UPDATE skill_packages
		SET status = 'released', released_by = $2, released_at = now(), signed_by = $3, signature = $4
		WHERE id = $1`, id, session.userID, sig.Kid, sig.Signature); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "release persist failed"))
		return
	}
	if err := s.skillAuditFail(w, r, "skill.released", name, version, audit.OutcomeAllowed,
		map[string]any{"released_by": session.userID, "kid": sig.Kid, "digest": digest}); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": skillreg.StatusReleased, "digest": digest,
		"signature": sig, "download": "/enterprise/v1/skills/" + name + "/versions/" + version + "/download",
	})
}

// handleSkillRevoke implements POST .../versions/{version}/revoke
// (skill.review at the skill's scope): any live status -> revoked; downloads then fail closed.
func (s *Server) handleSkillRevoke(w http.ResponseWriter, r *http.Request) {
	session, ok := s.skillDecision(w, r)
	if !ok {
		return
	}
	name, version := r.PathValue("name"), r.PathValue("version")
	tag, err := s.OIDCStore.Pool().Exec(r.Context(), `
		UPDATE skill_packages SET status = 'revoked', revoked_at = now()
		WHERE name = $1 AND version = $2 AND workspace_id = $3 AND status <> 'revoked'`,
		name, version, session.workspace)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "revoke failed"))
		return
	}
	if tag.RowsAffected() == 0 {
		writeProblem(w, r, errs.New(errs.NotFound, "no revocable version with this name@version"))
		return
	}
	if err := s.skillAuditFail(w, r, "skill.revoked", name, version, audit.OutcomeAllowed,
		map[string]any{"revoked_by": session.userID}); err != nil {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSkillDeprecate keeps an already released package verifiable for
// existing managed installations but removes it from new release selection.
func (s *Server) handleSkillDeprecate(w http.ResponseWriter, r *http.Request) {
	session, ok := s.skillDecision(w, r)
	if !ok {
		return
	}
	name, version := r.PathValue("name"), r.PathValue("version")
	tag, err := s.OIDCStore.Pool().Exec(r.Context(), `UPDATE skill_packages SET status = 'deprecated' WHERE name = $1 AND version = $2 AND workspace_id = $3 AND status = 'released'`, name, version, session.workspace)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "deprecate failed"))
		return
	}
	if tag.RowsAffected() == 0 {
		writeProblem(w, r, errs.New(errs.NotFound, "no released skill version to deprecate"))
		return
	}
	if err := s.skillAuditFail(w, r, "skill.deprecated", name, version, audit.OutcomeAllowed, map[string]any{"actor": session.userID}); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": skillreg.StatusDeprecated})
}

// handleSkillDownload implements GET .../versions/{version}/download:
// released packages only, workspace member only; bytes + digest + signature.
func (s *Server) handleSkillDownload(w http.ResponseWriter, r *http.Request) {
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	name, version := r.PathValue("name"), r.PathValue("version")
	var (
		payload   []byte
		digest    string
		signature string
		signedBy  string
		status    string
	)
	err = s.OIDCStore.Pool().QueryRow(r.Context(), `
		SELECT payload, digest, COALESCE(signature,''), COALESCE(signed_by,''), status
		FROM skill_packages
		WHERE name = $1 AND version = $2 AND workspace_id = $3`,
		name, version, session.Workspace.WorkspaceID).
		Scan(&payload, &digest, &signature, &signedBy, &status)
	if err != nil {
		// Cross-workspace or unknown: indistinguishable not-found.
		writeProblem(w, r, errs.New(errs.NotFound, "no such skill version"))
		return
	}
	if s.Skills != nil {
		v, err := s.Skills.GetVersion(r.Context(), session.Workspace.WorkspaceID, name, version)
		var g rbac.Grants
		if err == nil {
			g, err = s.grantsFor(r.Context(), session.Workspace.WorkspaceID, session.Principal.UserID, session.Workspace.Roles)
		}
		if err != nil || !mayDownload(skillshare.Viewer{UserID: session.Principal.UserID, Grants: g}, v) {
			writeProblem(w, r, errs.New(errs.NotFound, "skill version is not released"))
			return
		}
	} else if status != skillreg.StatusReleased && status != skillreg.StatusDeprecated {
		// Deprecated stays downloadable; revoked/pending/approved do not.
		writeProblem(w, r, errs.New(errs.NotFound, "skill version is not released"))
		return
	}
	w.Header().Set("X-Skill-Digest", "sha256-"+digest)
	// 签名以 JSON envelope 下发（kid/digest/signature/alg），支持离线验证。
	envelope := skillreg.Signature{
		Kid: signedBy, Alg: "EdDSA", Digest: digest, Signature: signature,
	}
	if sigJSON, err := json.Marshal(envelope); err == nil {
		w.Header().Set("X-Skill-Signature", string(sigJSON))
	}
	w.Header().Set("Content-Type", "application/gzip")
	_, _ = w.Write(payload)
}

// handlePackageKeys serves the public package-key discovery document
// (active + historical keys; never private material).
func (s *Server) handlePackageKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.ListPackageKeys(r.Context())
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, skillreg.KeySetJWK(keys))
}

// --- helpers ---

type identities = gatewayIdentities

// activePackageKey loads the active pkg- key (public from store, private
// injected at wiring).
func (s *Server) activePackageKey(ctx context.Context) (pkgSigningKey, error) {
	kid, err := s.Store.ActivePackageKeyID(ctx)
	if err != nil {
		return pkgSigningKey{}, err
	}
	priv, ok := s.PackageKeys[kid]
	if !ok {
		return pkgSigningKey{}, errs.New(errs.Internal, "active package key is not loaded in this instance")
	}
	return pkgSigningKey{kid: kid, priv: priv}, nil
}

// pkgSigningKey is one loaded private release-signing key.
type pkgSigningKey struct {
	kid  string
	priv ed25519.PrivateKey
}

func (s *Server) skillAuditFail(w http.ResponseWriter, r *http.Request, eventType, name, version string, outcome audit.Outcome, detail map[string]any) error {
	ev := audit.NewEvent(audit.EventType(eventType), outcome)
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Resource = "skill/" + name + "@" + version
	ev.Detail = detail
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		log.Printf("gateway: SECURITY AUDIT delivery failure: %v", err)
		writeProblem(w, r, errs.New(errs.Internal, "transition could not be audited"))
		return err
	}
	return nil
}

func (s *Server) requireMaintainer(w http.ResponseWriter, r *http.Request) (gatewayIdentities, bool) {
	return s.requirePermission(w, r, rbac.SkillReview)
}

func validSkillName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validSkillVersion(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 8 {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

var _ = time.Time{}

// handleSkillCatalog implements GET /enterprise/v1/skills: the server-driven
// catalog for the Portal Marketplace (workspace-scoped; no payload, no
// storage data). Central policy hides denied skills, consistent with CLI.
func (s *Server) handleSkillCatalog(w http.ResponseWriter, r *http.Request) {
	// Read-only catalog: session JWT (Portal) or agk key (CLI) both work.
	auth, ok := s.requireCatalogAuth(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, err := s.OIDCStore.Pool().Query(r.Context(), `
		SELECT name, version, status, digest
		FROM skill_packages
		WHERE workspace_id = $1 AND ($2 = '' OR name LIKE '%' || $2 || '%')
		  AND status IN ('pending_review','approved','released','deprecated')
		  AND (visibility = 'organization' OR published_by = $3)
		ORDER BY name, version DESC`,
		session.Workspace.WorkspaceID, query, session.Principal.UserID)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "skill catalog lookup failed"))
		return
	}
	defer rows.Close()
	skills := []map[string]any{}
	for rows.Next() {
		var name, version, status, digest string
		if err := rows.Scan(&name, &version, &status, &digest); err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "skill catalog scan failed"))
			return
		}
		skills = append(skills, map[string]any{
			"name": name, "version": version, "status": status, "digest": digest,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
}

// publishShared stores a version through packages/skillshare: scope,
// protected SKILL.md, and the audit event in the same transaction.
func (s *Server) publishShared(w http.ResponseWriter, r *http.Request, session identity.Session, name, version, visibility, groupID string, protected bool, files []skillreg.PackageFile) {
	if visibility == "" {
		visibility = skillshare.VisibilityOrganization
	}
	g, err := s.grantsFor(r.Context(), session.Workspace.WorkspaceID, session.Principal.UserID, session.Workspace.Roles)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "permission lookup failed"))
		return
	}
	ev := s.skillEvent(r, session, "skill.published")
	out, err := s.Skills.Publish(r.Context(), session.Workspace.WorkspaceID, skillshare.Viewer{UserID: session.Principal.UserID, Grants: g},
		skillshare.Publish{Name: name, Version: version, Visibility: visibility, GroupID: strings.TrimSpace(groupID), Protected: protected, Files: files}, ev)
	if err != nil {
		writeProblem(w, r, skillProblem(err))
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
