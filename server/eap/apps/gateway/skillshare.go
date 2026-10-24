package main

// Skill sharing surface (packages/skillshare): a member's skill shelf,
// enabling and requiring skills, the review queue, reading a version for
// review, and rejecting one. Publishing, approval and release stay on the
// registry routes in skills.go, which consult the same scope rules.
//
// Invariants: every decision is made against server-side facts (version
// scope and author, the caller's live grants); a protected SKILL.md leaves
// the server only to its author or a reviewer at its scope (audited), and to
// the model through the inference routes — never through a download.

import (
	"errors"
	"net/http"
	"strings"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillshare"
)

func skillProblem(err error) error {
	var p *errs.Error
	switch {
	case errors.As(err, &p):
		return err
	case errors.Is(err, skillshare.ErrInvalid):
		return errs.New(errs.InvalidArgument, err.Error())
	case errors.Is(err, skillshare.ErrForbidden):
		return errs.New(errs.PermissionDenied, "skill action is not permitted")
	case errors.Is(err, skillshare.ErrNotFound):
		return errs.New(errs.NotFound, "no such skill")
	case errors.Is(err, skillshare.ErrConflict):
		return errs.New(errs.Conflict, err.Error())
	default:
		return errs.Wrap(err, errs.Internal, "skill storage failed")
	}
}

// skillCaller resolves the member (session or employee key) and their grants.
func (s *Server) skillCaller(w http.ResponseWriter, r *http.Request) (identity.Session, skillshare.Viewer, bool) {
	c, _, ok := s.requireContentCaller(w, r)
	if !ok {
		return identity.Session{}, skillshare.Viewer{}, false
	}
	return c.session, skillshare.Viewer{UserID: c.actor.UserID, Grants: c.actor.Grants}, true
}

func (s *Server) skillEvent(r *http.Request, session identity.Session, t audit.EventType) audit.Event {
	ev := audit.NewEvent(t, audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.Principal.UserID}
	ev.WorkspaceID = session.Workspace.WorkspaceID
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	return ev
}

// handleMySkills implements GET /enterprise/v1/me/skills.
func (s *Server) handleMySkills(w http.ResponseWriter, r *http.Request) {
	session, viewer, ok := s.skillCaller(w, r)
	if !ok {
		return
	}
	mine, err := s.Skills.Mine(r.Context(), session.Workspace.WorkspaceID, viewer)
	if err != nil {
		writeProblem(w, r, skillProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": mine})
}

// handleEnableSkill implements PUT and DELETE /enterprise/v1/me/skills/{name}.
func (s *Server) handleEnableSkill(w http.ResponseWriter, r *http.Request) {
	session, viewer, ok := s.skillCaller(w, r)
	if !ok {
		return
	}
	on := r.Method == http.MethodPut
	ev := s.skillEvent(r, session, "skill.enablement")
	if err := s.Skills.SetEnabled(r.Context(), session.Workspace.WorkspaceID, viewer, r.PathValue("name"), on, ev); err != nil {
		writeProblem(w, r, skillProblem(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRequireSkill implements PUT and DELETE
// /enterprise/v1/skills/{name}/requirement?group_id= (no group_id = whole
// Workspace).
func (s *Server) handleRequireSkill(w http.ResponseWriter, r *http.Request) {
	session, viewer, ok := s.skillCaller(w, r)
	if !ok {
		return
	}
	on := r.Method == http.MethodPut
	groupID := strings.TrimSpace(r.URL.Query().Get("group_id"))
	ev := s.skillEvent(r, session, "skill.requirement")
	if err := s.Skills.SetRequired(r.Context(), session.Workspace.WorkspaceID, viewer, r.PathValue("name"), groupID, on, ev); err != nil {
		writeProblem(w, r, skillProblem(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSkillReviewQueue implements GET /enterprise/v1/skill-reviews.
func (s *Server) handleSkillReviewQueue(w http.ResponseWriter, r *http.Request) {
	session, viewer, ok := s.skillCaller(w, r)
	if !ok {
		return
	}
	pending, err := s.Skills.Pending(r.Context(), session.Workspace.WorkspaceID, viewer)
	if err != nil {
		writeProblem(w, r, skillProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": pending})
}

// handleSkillContent implements GET
// /enterprise/v1/skills/{name}/versions/{version}/content: SKILL.md and the
// resource file list, for the author and for reviewers at the skill's scope.
func (s *Server) handleSkillContent(w http.ResponseWriter, r *http.Request) {
	session, viewer, ok := s.skillCaller(w, r)
	if !ok {
		return
	}
	v, err := s.Skills.GetVersion(r.Context(), session.Workspace.WorkspaceID, r.PathValue("name"), r.PathValue("version"))
	if err == nil && !skillshare.MayRead(viewer, v) {
		err = skillshare.ErrNotFound
	}
	if err != nil {
		writeProblem(w, r, skillProblem(err))
		return
	}
	content, err := s.Skills.Content(r.Context(), v)
	if err != nil {
		writeProblem(w, r, skillProblem(err))
		return
	}
	ev := s.skillEvent(r, session, "skill.content_viewed")
	ev.Resource = "skill/" + v.Name + "@" + v.Version
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "audit delivery failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": v.Name, "version": v.Version, "status": v.Status, "visibility": v.Visibility,
		"group_id": v.GroupID, "protected": v.Protected, "description": v.Description,
		"skill_md": content.SkillMD, "files": content.Files,
	})
}

// handleSkillReject implements POST .../versions/{version}/reject.
func (s *Server) handleSkillReject(w http.ResponseWriter, r *http.Request) {
	session, viewer, ok := s.skillCaller(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	v, err := s.Skills.GetVersion(r.Context(), session.Workspace.WorkspaceID, r.PathValue("name"), r.PathValue("version"))
	if err == nil {
		err = s.Skills.Reject(r.Context(), session.Workspace.WorkspaceID, viewer, v, body.Reason, s.skillEvent(r, session, "skill.rejected"))
	}
	if err != nil {
		writeProblem(w, r, skillProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": skillshare.StatusRejected})
}

// skillDecision loads a version and checks the control-plane caller may
// decide on it at its scope (registry review/release/deprecate/revoke).
func (s *Server) skillDecision(w http.ResponseWriter, r *http.Request) (gatewayIdentities, bool) {
	if s.Skills == nil {
		return s.requireMaintainer(w, r)
	}
	session, ok := s.requireControlPlane(w, r)
	if !ok {
		return gatewayIdentities{}, false
	}
	v, err := s.Skills.GetVersion(r.Context(), session.workspace, r.PathValue("name"), r.PathValue("version"))
	if err != nil {
		writeProblem(w, r, skillProblem(err))
		return gatewayIdentities{}, false
	}
	if v.Visibility == skillshare.VisibilityPersonal {
		writeProblem(w, r, errs.New(errs.Conflict, "personal skills are not reviewed"))
		return gatewayIdentities{}, false
	}
	if !skillshare.MayReview(skillshare.Viewer{UserID: session.userID, Grants: session.grants}, v) {
		writeProblem(w, r, errs.New(errs.PermissionDenied, "skill.review at the skill's scope is required, and never on your own skill"))
		return gatewayIdentities{}, false
	}
	return session, true
}

// mayDownload decides resource downloads: released (or deprecated, for
// existing installations) at a scope that reaches the caller, or the
// author's own personal version.
func mayDownload(viewer skillshare.Viewer, v skillshare.Version) bool {
	live := v.Status == skillreg.StatusReleased || v.Status == skillreg.StatusDeprecated
	switch v.Visibility {
	case skillshare.VisibilityOrganization:
		return live
	case skillshare.VisibilityTeam:
		return live && v.GroupID != "" && v.GroupID == viewer.Grants.GroupID
	case skillshare.VisibilityPersonal:
		return v.AuthorID == viewer.UserID && v.Status == skillshare.StatusPrivate
	}
	return false
}
