package main

// Curation proposals (/enterprise/v1/curation/proposals): reviewers see what
// the curation worker proposed or applied, accept or reject review proposals
// (the same as approving the submitted revision in the review queue) and
// revert auto proposals.
//
// Pitfalls: a proposal the caller may not decide reads as not_found, like
// content; leads only see proposals on their own team's pages.

import (
	"errors"
	"net/http"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/curation"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/rbac"
)

func curationProblem(err error) error {
	switch {
	case errors.Is(err, curation.ErrInvalid):
		return errs.New(errs.InvalidArgument, "decision is accept, reject or revert")
	case errors.Is(err, curation.ErrNotFound):
		return errs.New(errs.NotFound, "proposal not found")
	case errors.Is(err, curation.ErrForbidden):
		return errs.New(errs.PermissionDenied, "proposal decision is not permitted")
	case errors.Is(err, curation.ErrConflict):
		return errs.New(errs.Conflict, "the proposal is no longer in a state that allows this decision")
	default:
		return errs.Wrap(err, errs.Internal, "curation storage failed")
	}
}

// handleListProposals implements GET /enterprise/v1/curation/proposals?status=.
func (s *Server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	items, err := s.Curation.List(r.Context(), c.session.Workspace.WorkspaceID, c.actor, r.URL.Query().Get("status"))
	if err != nil {
		writeProblem(w, r, curationProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": items})
}

// handleDecideProposal implements POST /enterprise/v1/curation/proposals/{id}/decision.
func (s *Server) handleDecideProposal(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	var body struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	ev := s.contentEvent(r, c)
	if body.Comment != "" {
		ev.Detail = map[string]any{"comment": body.Comment}
	}
	p, err := s.Curation.Decide(r.Context(), c.session.Workspace.WorkspaceID, c.actor, r.PathValue("id"), body.Decision, ev)
	if err != nil {
		writeProblem(w, r, curationProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleRequestSweep implements POST /enterprise/v1/curation/sweeps (admin):
// queue a global re-curation of the admin's Workspace now instead of at the
// curator's next scheduled sweep.
func (s *Server) handleRequestSweep(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.CurationSweep)
	if !ok {
		return
	}
	ev := s.adminEvent(r, session, "curation.sweep_requested", "workspace/"+session.workspace, nil)
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "audit delivery failed — sweep not queued"))
		return
	}
	queued, err := s.Curation.EnqueueSweep(r.Context(), session.workspace)
	if err != nil {
		writeProblem(w, r, curationProblem(err))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued})
}
