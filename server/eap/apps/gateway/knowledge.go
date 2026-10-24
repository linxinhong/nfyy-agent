package main

import (
	"encoding/json"
	"net/http"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/knowledge"
)

// handleKnowledgeUpload implements POST /enterprise/v1/knowledge/documents:
// employee key + workspace membership -> DLP scan of the document -> ingest
// (parse/chunk/embed/index, idempotent). The DLP scan failure blocks the
// upload; raw content never enters logs or audit.
func (s *Server) handleKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	if s.Knowledge == nil || s.DLP == nil {
		writeProblem(w, r, errs.New(errs.NotFound, "knowledge is not configured"))
		return
	}
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	r = withDLPSubject(r, session.Workspace.WorkspaceID, session.Workspace.Roles, false)
	var body struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	if body.Title == "" || body.Content == "" {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "title and content are required"))
		return
	}
	// Pre-ingest DLP scan (enforce): deny-class content never enters the index.
	scan, err := s.DLP.Scan(r.Context(), body.Content)
	if err != nil {
		writeProblem(w, r, errs.DLPFailClosed(err.Error()))
		return
	}
	if len(scan.RuleNames) > 0 {
		outcome := audit.OutcomeAllowed
		if scan.Blocked {
			outcome = audit.OutcomeDenied
		}
		ev := audit.NewEvent("dlp.hit", outcome)
		ev.Actor = &identity.Principal{UserID: session.Principal.UserID}
		ev.RequestID = requestIDOf(r)
		ev.TraceID = traceIDOf(r)
		ev.Resource = "knowledge/" + body.Title
		ev.Detail = map[string]any{
			"rules": scan.RuleNames, "surface": "knowledge.upload",
			"alert_only": !scan.Blocked,
		}
		if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "dlp audit delivery failed"))
			return
		}
	}
	if scan.Blocked {
		writeProblem(w, r, errs.New(errs.DLPBlocked, "document blocked by DLP: "+scan.Reason))
		return
	}
	// Redact-class matches: the REDACTED content is what enters the index —
	// the raw form is never persisted.
	stored := body.Content
	if scan.RedactionCount > 0 {
		stored = scan.Redacted
	}
	result, err := s.Knowledge.Ingest(r.Context(), session.Workspace.WorkspaceID, body.Title, stored, session.Principal.UserID)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"document_id":       result.Document.ID,
		"title":             result.Document.Title,
		"version":           result.Document.Version,
		"status":            result.Document.Status,
		"idempotent_replay": result.IdempotentReplay,
		"redactions":        scan.RedactionCount,
	})
}

// handleKnowledgeSearch implements POST /enterprise/v1/knowledge/search:
// workspace-scoped retrieval with mandatory citations; the query passes the
// DLP gate (search strings can carry secrets too).
func (s *Server) handleKnowledgeSearch(w http.ResponseWriter, r *http.Request) {
	if s.Knowledge == nil || s.DLP == nil {
		writeProblem(w, r, errs.New(errs.NotFound, "knowledge is not configured"))
		return
	}
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	r = withDLPSubject(r, session.Workspace.WorkspaceID, session.Workspace.Roles, false)
	var body struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k,omitempty"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	scan, err := s.DLP.Scan(r.Context(), body.Query)
	if err != nil {
		writeProblem(w, r, errs.DLPFailClosed(err.Error()))
		return
	}
	if scan.Blocked {
		writeProblem(w, r, errs.New(errs.DLPBlocked, "query blocked by DLP: "+scan.Reason))
		return
	}
	// Redact-class query matches search the redacted form.
	query := body.Query
	if scan.RedactionCount > 0 {
		query = scan.Redacted
	}
	hits, err := s.Knowledge.Search(r.Context(), session.Workspace.WorkspaceID, query, body.TopK)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	// Outbound egress scan on every returned chunk: deny-class content is
	// withheld (citation metadata survives), redact-class content is
	// released in redacted form. This also covers chunks stored before the
	// current rules existed.
	results := make([]map[string]any, 0, len(hits))
	var egressRules []string
	withheld := 0
	for _, hit := range hits {
		content := hit.Content
		res, err := s.DLP.Scan(r.Context(), content)
		if err != nil {
			writeProblem(w, r, errs.DLPFailClosed(err.Error()))
			return
		}
		if len(res.RuleNames) > 0 {
			egressRules = append(egressRules, res.RuleNames...)
		}
		if res.Blocked {
			withheld++
			content = "[WITHHELD:DLP] " + res.Reason
		} else if res.RedactionCount > 0 {
			content = res.Redacted
		}
		results = append(results, map[string]any{
			"document_id": hit.DocumentID,
			"title":       hit.Title,
			"version":     hit.Version,
			"seq":         hit.Seq,
			"score":       hit.Score,
			"content":     content,
		})
	}
	// Exempt subjects see originals: pseudonyms stored by other users'
	// uploads are restored from the vault per result.
	if s.subjectExempt(r, session.Workspace.Roles) {
		for i := range results {
			content, _ := results[i]["content"].(string)
			if restored, n := s.resubstituteTokens(r.Context(), session.Workspace.WorkspaceID, content); n > 0 {
				results[i]["content"] = restored
			}
		}
	}
	if len(egressRules) > 0 {
		outcome := audit.OutcomeAllowed
		if withheld > 0 {
			outcome = audit.OutcomeDenied
		}
		ev := audit.NewEvent("dlp.hit", outcome)
		ev.Actor = &identity.Principal{UserID: session.Principal.UserID}
		ev.RequestID = requestIDOf(r)
		ev.TraceID = traceIDOf(r)
		ev.Resource = "knowledge/search"
		ev.Detail = map[string]any{
			"rules": egressRules, "surface": "knowledge.search_results",
			"withheld": withheld, "results": len(results),
		}
		if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
			writeProblem(w, r, errs.Wrap(err, errs.Internal, "dlp audit delivery failed"))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

var _ = json.Marshal
var _ = dlp.Scanner{}
var _ = knowledge.Citation{}
