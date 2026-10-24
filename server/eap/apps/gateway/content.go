package main

// Shared content API (news, Wiki, capability notes) under /enterprise/v1/resources.
//
// Callers authenticate with a control-plane session (Portal) or an employee
// key (desktop, agents); the Workspace comes from the frozen selection rules
// and membership is re-checked live on every request. Resource rules live in
// packages/content (Allowed); this layer only resolves the actor, gates
// writes through DLP and maps errors onto the frozen contract.
//
// Pitfalls:
//   - Writes fail closed without a DLP engine: stored content is knowledge
//     and falls under the Knowledge DLP constraint.
//   - Redact-class matches store the redacted text; the raw form is never
//     persisted. Deny-class matches refuse the write.

import (
	"errors"
	"net/http"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
)

const maxContentBody = 256 << 10

func contentProblem(err error) error {
	switch {
	case errors.Is(err, content.ErrInvalid):
		return errs.New(errs.InvalidArgument, "invalid resource fields")
	case errors.Is(err, content.ErrForbidden):
		return errs.New(errs.PermissionDenied, "resource action is not permitted")
	case errors.Is(err, content.ErrNotFound):
		return errs.New(errs.NotFound, "resource not found")
	case errors.Is(err, content.ErrConflict):
		return errs.New(errs.Conflict, "resource state has changed")
	default:
		return errs.Wrap(err, errs.Internal, "content storage failed")
	}
}

// contentCaller is the authenticated member inside the selected Workspace.
type contentCaller struct {
	session identity.Session
	actor   content.Actor
}

func (s *Server) requireContentCaller(w http.ResponseWriter, r *http.Request) (contentCaller, *http.Request, bool) {
	auth, ok := s.requireCatalogAuth(w, r)
	if !ok {
		return contentCaller{}, r, false
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return contentCaller{}, r, false
	}
	actor, err := s.Content.ActorFor(r.Context(), session.Workspace.WorkspaceID, session.Principal.UserID, session.Workspace.Roles)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "member lookup failed"))
		return contentCaller{}, r, false
	}
	r = withDLPSubject(r, session.Workspace.WorkspaceID, session.Workspace.Roles, false)
	return contentCaller{session: session, actor: actor}, r, true
}

func (s *Server) contentEvent(r *http.Request, c contentCaller) audit.Event {
	ev := audit.NewEvent("content", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: c.session.Principal.UserID}
	ev.WorkspaceID = c.session.Workspace.WorkspaceID
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	return ev
}

// scanContent passes every free-text field through DLP, replacing redact-class
// matches in place. A deny-class match or engine failure refuses the write.
func (s *Server) scanContent(r *http.Request, c contentCaller, in *content.Input) error {
	if s.DLP == nil {
		return errs.DLPFailClosed("content DLP engine is not configured")
	}
	var rules []string
	for _, field := range []*string{&in.Title, &in.Summary, &in.Body} {
		if *field == "" {
			continue
		}
		scan, err := s.DLP.Scan(r.Context(), *field)
		if err != nil {
			return errs.DLPFailClosed(err.Error())
		}
		rules = append(rules, scan.RuleNames...)
		if scan.Blocked {
			if err := s.auditContentDLP(r, c, rules, true); err != nil {
				return err
			}
			return errs.New(errs.DLPBlocked, "content blocked by DLP: "+scan.Reason)
		}
		if scan.RedactionCount > 0 {
			*field = scan.Redacted
		}
	}
	if len(rules) > 0 {
		return s.auditContentDLP(r, c, rules, false)
	}
	return nil
}

func (s *Server) auditContentDLP(r *http.Request, c contentCaller, rules []string, blocked bool) error {
	outcome := audit.OutcomeAllowed
	if blocked {
		outcome = audit.OutcomeDenied
	}
	ev := audit.NewEvent("dlp.hit", outcome)
	ev.Actor = &identity.Principal{UserID: c.session.Principal.UserID}
	ev.WorkspaceID = c.session.Workspace.WorkspaceID
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Detail = map[string]any{"rules": rules, "surface": "content.write", "alert_only": !blocked}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		return errs.Wrap(err, errs.Internal, "dlp audit delivery failed")
	}
	return nil
}

func (s *Server) decodeContentInput(w http.ResponseWriter, r *http.Request, c contentCaller) (content.Input, bool) {
	var in content.Input
	r.Body = http.MaxBytesReader(w, r.Body, maxContentBody)
	if err := decodeJSONBody(w, r, &in); err != nil {
		writeProblem(w, r, err)
		return content.Input{}, false
	}
	if err := s.scanContent(r, c, &in); err != nil {
		writeProblem(w, r, err)
		return content.Input{}, false
	}
	return in, true
}

// handleListResources implements GET /enterprise/v1/resources.
func (s *Server) handleListResources(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	items, err := s.Content.List(r.Context(), c.session.Workspace.WorkspaceID, c.actor,
		r.URL.Query().Get("kind"), r.URL.Query().Get("q"))
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": items})
}

// handleCreateResource implements POST /enterprise/v1/resources.
func (s *Server) handleCreateResource(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	in, ok := s.decodeContentInput(w, r, c)
	if !ok {
		return
	}
	item, err := s.Content.Create(r.Context(), c.session.Workspace.WorkspaceID, c.actor, in, s.contentEvent(r, c))
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

// handleGetResource implements GET /enterprise/v1/resources/{id}.
func (s *Server) handleGetResource(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	item, err := s.Content.Get(r.Context(), c.session.Workspace.WorkspaceID, c.actor, r.PathValue("id"))
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// handleEditResource implements PUT /enterprise/v1/resources/{id}.
func (s *Server) handleEditResource(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	in, ok := s.decodeContentInput(w, r, c)
	if !ok {
		return
	}
	item, err := s.Content.Edit(r.Context(), c.session.Workspace.WorkspaceID, c.actor, r.PathValue("id"), in, s.contentEvent(r, c))
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// handleResourceTransition implements POST /enterprise/v1/resources/{id}/{submit|approve|reject|withdraw}.
func (s *Server) handleResourceTransition(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, r, ok := s.requireContentCaller(w, r)
		if !ok {
			return
		}
		item, err := s.Content.Transition(r.Context(), c.session.Workspace.WorkspaceID, c.actor, r.PathValue("id"), action, s.contentEvent(r, c))
		if err != nil {
			writeProblem(w, r, contentProblem(err))
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}

// handleReviewQueue implements GET /enterprise/v1/review/queue.
func (s *Server) handleReviewQueue(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	items, err := s.Content.ReviewQueue(r.Context(), c.session.Workspace.WorkspaceID, c.actor)
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": items})
}
