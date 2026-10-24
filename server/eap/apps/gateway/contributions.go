package main

// Contribution inbox API (/enterprise/v1/contributions): local agents upload
// redacted page snapshots; the curation layer works from the stored versions.
//
// Why: uploads are machine-written, so every refusal is a 400 with the frozen
// problem body plus `reasons` (field, code, message) the agent can act on,
// and replays are harmless (Idempotency-Key + server-side content hash).
//
// Pitfalls:
//   - Any DLP hit — deny or redact class — returns the upload to the author
//     as sensitive_content. Unlike content writes, nothing is redacted on the
//     server: the author declared the page redacted, so a hit means it is not.
//   - Rejections are audited with reason codes only; rejected text is never
//     stored or logged.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/contribution"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/rbac"
)

const (
	maxContributionBody = 512 << 10
	contributionPage    = 100
)

// rejectionBody is the frozen problem shape plus actionable reasons.
type rejectionBody struct {
	errs.Problem
	Reasons []contribution.Reason `json:"reasons"`
}

func (s *Server) writeRejection(w http.ResponseWriter, r *http.Request, author string, workspaceID string, reasons []contribution.Reason) {
	codes := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		codes = append(codes, reason.Field+":"+reason.Code)
	}
	ev := audit.NewEvent("contribution.rejected", audit.OutcomeDenied)
	ev.Actor = &identity.Principal{UserID: author}
	ev.WorkspaceID = workspaceID
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Detail = map[string]any{"reasons": codes}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "audit delivery failed"))
		return
	}
	body := rejectionBody{Problem: errs.Problem{Code: errs.InvalidArgument, Message: "contribution rejected", TraceID: traceIDOf(r)}, Reasons: reasons}
	writeJSON(w, http.StatusBadRequest, body)
}

func contributionProblem(err error) error {
	switch {
	case errors.Is(err, contribution.ErrNotFound):
		return errs.New(errs.NotFound, "contribution not found")
	case errors.Is(err, contribution.ErrIdempotencyMismatch):
		return errs.New(errs.Conflict, "Idempotency-Key was already used for a different upload")
	case errors.Is(err, contribution.ErrConflict):
		return errs.New(errs.Conflict, "the same local page is being uploaded concurrently; retry").AsRetriable()
	default:
		return errs.Wrap(err, errs.Internal, "contribution storage failed")
	}
}

// contributionAuthor authenticates a member (session or employee key) in the selected Workspace.
func (s *Server) contributionAuthor(w http.ResponseWriter, r *http.Request) (identity.Session, contribution.Author, *http.Request, bool) {
	auth, ok := s.requireCatalogAuth(w, r)
	if !ok {
		return identity.Session{}, contribution.Author{}, r, false
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return identity.Session{}, contribution.Author{}, r, false
	}
	actor, err := s.Content.ActorFor(r.Context(), session.Workspace.WorkspaceID, session.Principal.UserID, session.Workspace.Roles)
	if err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "member lookup failed"))
		return identity.Session{}, contribution.Author{}, r, false
	}
	r = withDLPSubject(r, session.Workspace.WorkspaceID, session.Workspace.Roles, false)
	return session, contribution.Author{UserID: actor.UserID, AnyTeam: actor.Grants.Has(rbac.KnowledgeManage), GroupID: actor.GroupID}, r, true
}

func (s *Server) contributionEvent(r *http.Request, session identity.Session) audit.Event {
	ev := audit.NewEvent("contribution", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.Principal.UserID}
	ev.WorkspaceID = session.Workspace.WorkspaceID
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	return ev
}

// scanContribution reports DLP hits per free-text field; it never rewrites text.
func (s *Server) scanContribution(r *http.Request, u contribution.Upload) ([]contribution.Reason, error) {
	if s.DLP == nil {
		return nil, errs.DLPFailClosed("contribution DLP engine is not configured")
	}
	var reasons []contribution.Reason
	for _, f := range []struct{ name, text string }{{"title", u.Title}, {"summary", u.Summary}, {"markdown", u.Markdown}} {
		if f.text == "" {
			continue
		}
		scan, err := s.DLP.Scan(r.Context(), f.text)
		if err != nil {
			return nil, errs.DLPFailClosed(err.Error())
		}
		if len(scan.RuleNames) > 0 {
			reasons = append(reasons, contribution.Reason{Field: f.name, Code: contribution.ReasonSensitiveContent,
				Message: "sensitive content detected; redact it locally and upload again", Rules: scan.RuleNames})
		}
	}
	return reasons, nil
}

func validIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// handleUploadContribution implements POST /enterprise/v1/contributions.
func (s *Server) handleUploadContribution(w http.ResponseWriter, r *http.Request) {
	session, author, r, ok := s.contributionAuthor(w, r)
	if !ok {
		return
	}
	workspaceID := session.Workspace.WorkspaceID
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validIdempotencyKey(key) {
		s.writeRejection(w, r, author.UserID, workspaceID, []contribution.Reason{{Field: "Idempotency-Key", Code: contribution.ReasonRequired,
			Message: "send a unique Idempotency-Key header (1-128 visible ASCII characters) per upload attempt"}})
		return
	}
	var u contribution.Upload
	r.Body = http.MaxBytesReader(w, r.Body, maxContributionBody)
	if err := decodeJSONBody(w, r, &u); err != nil {
		writeProblem(w, r, err)
		return
	}
	contribution.Normalize(&u)
	reasons := contribution.Validate(u)
	dlpReasons, err := s.scanContribution(r, u)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if reasons = append(reasons, dlpReasons...); len(reasons) > 0 {
		s.writeRejection(w, r, author.UserID, workspaceID, reasons)
		return
	}
	c, err := s.Contributions.Upload(r.Context(), workspaceID, author, key, u, s.contributionEvent(r, session))
	var rejected *contribution.RejectedError
	if errors.As(err, &rejected) {
		s.writeRejection(w, r, author.UserID, workspaceID, rejected.Reasons)
		return
	}
	if err != nil {
		writeProblem(w, r, contributionProblem(err))
		return
	}
	status := http.StatusCreated
	if c.IdempotentReplay {
		status = http.StatusOK
	}
	writeJSON(w, status, c)
}

// handleListContributions implements GET /enterprise/v1/contributions?since=&limit=.
func (s *Server) handleListContributions(w http.ResponseWriter, r *http.Request) {
	session, author, r, ok := s.contributionAuthor(w, r)
	if !ok {
		return
	}
	since := time.Unix(0, 0).UTC()
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeProblem(w, r, errs.New(errs.InvalidArgument, "since must be an RFC 3339 timestamp"))
			return
		}
		since = parsed
	}
	limit := contributionPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > contributionPage {
			writeProblem(w, r, errs.New(errs.InvalidArgument, "limit must be 1-100"))
			return
		}
		limit = n
	}
	items, err := s.Contributions.List(r.Context(), session.Workspace.WorkspaceID, author.UserID, since, limit)
	if err != nil {
		writeProblem(w, r, contributionProblem(err))
		return
	}
	body := map[string]any{"contributions": items}
	if len(items) == limit {
		body["next_since"] = items[len(items)-1].UpdatedAt.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, body)
}

// handleGetContribution implements GET /enterprise/v1/contributions/{id}.
func (s *Server) handleGetContribution(w http.ResponseWriter, r *http.Request) {
	session, author, r, ok := s.contributionAuthor(w, r)
	if !ok {
		return
	}
	c, err := s.Contributions.Get(r.Context(), session.Workspace.WorkspaceID, author.UserID, r.PathValue("id"))
	if err != nil {
		writeProblem(w, r, contributionProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleWithdrawContribution implements POST /enterprise/v1/contributions/{id}/withdraw.
func (s *Server) handleWithdrawContribution(w http.ResponseWriter, r *http.Request) {
	session, author, r, ok := s.contributionAuthor(w, r)
	if !ok {
		return
	}
	c, err := s.Contributions.Withdraw(r.Context(), session.Workspace.WorkspaceID, author.UserID, r.PathValue("id"), s.contributionEvent(r, session))
	if err != nil {
		writeProblem(w, r, contributionProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, c)
}
