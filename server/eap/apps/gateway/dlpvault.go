package main

// Reversible DLP reveal (TASK-028): authorized workspace members restore
// originals behind [rule:token] pseudonyms at display time. Every call is
// audited on the reliable path; foreign-workspace and unknown tokens are
// indistinguishable (fail closed).

import (
	"context"
	"log"
	"net/http"
	"regexp"

	"enterprise-agent-platform/packages/dlp"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/store"
)

// dlpWorkspaceKey carries the request's workspace into the DLP engine's
// Tokenizer callback (vault rows are workspace-scoped).
type dlpWorkspaceKey struct{}

// withDLPWorkspace binds the workspace onto the request context so every
// downstream scan (LLM router, knowledge planes) tokenizes into the right
// vault scope.
func withDLPWorkspace(r *http.Request, workspaceID string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), dlpWorkspaceKey{}, workspaceID))
}

// withDLPSubject binds workspace + subject (roles, provider/employee
// surface) for subject-aware scans. Provider egress MUST use
// forProvider=true so role exemptions never weaken the outbound defense.
func withDLPSubject(r *http.Request, workspaceID string, roles []string, forProvider bool) *http.Request {
	ctx := context.WithValue(r.Context(), dlpWorkspaceKey{}, workspaceID)
	ctx = dlp.ContextWithSubject(ctx, dlp.Subject{Roles: roles, ForProvider: forProvider})
	ctx = dlp.ContextWithWorkspace(ctx, workspaceID)
	return r.WithContext(ctx)
}

// dlpScanEmployeeText runs an employee-facing scan over one text blob:
// exempt subjects see originals (tokens re-substituted), deny-class content
// is withheld, redact/tokenize rewrite. Failures fail closed to a marker.
func (s *Server) dlpScanEmployeeText(r *http.Request, session identity.Session, text string) string {
	if s.DLP == nil {
		return text
	}
	r = withDLPSubject(r, session.Workspace.WorkspaceID, session.Workspace.Roles, false)
	res, err := s.DLP.Scan(r.Context(), text)
	if err != nil {
		return "[WITHHELD:DLP] scan unavailable"
	}
	if res.Blocked {
		return "[WITHHELD:DLP] " + res.Reason
	}
	out := res.Redacted
	if s.subjectExempt(r, session.Workspace.Roles) {
		if restored, n := s.resubstituteTokens(r.Context(), session.Workspace.WorkspaceID, out); n > 0 {
			out = restored
		}
	}
	return out
}

// subjectExempt reports whether the caller's roles intersect any DLP rule's
// except_roles (employee-facing surfaces only). A scan failure is treated as
// NOT exempt (fail closed).
func (s *Server) subjectExempt(r *http.Request, roles []string) bool {
	scanner, ok := s.DLP.(*dlp.TableScanner)
	if !ok {
		return false
	}
	rules, err := scanner.AllRules(r.Context())
	if err != nil {
		return false
	}
	for _, rule := range rules {
		if len(rule.ExceptRoles) > 0 && rolesIntersectStrings(roles, rule.ExceptRoles) {
			return true
		}
	}
	return false
}

func rolesIntersectStrings(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// dlpTokenPattern matches [rule:token] pseudonyms in released content.
var dlpTokenPattern = regexp.MustCompile(`\[([A-Za-z0-9_]+):([A-Za-z0-9]{8,32})\]`)

// resubstituteTokens replaces [rule:token] pseudonyms with originals from
// the vault for callers entitled to see them. Unknown tokens are left
// untouched (fail closed to the pseudonym). All pseudonyms resolve in one
// batched vault read (TASK-037) when the batch seam is wired; the store
// fallback keeps per-token reveals for unwired constructions.
func (s *Server) resubstituteTokens(ctx context.Context, workspaceID string, content string) (string, int) {
	matches := dlpTokenPattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return content, 0
	}
	revealed := make(map[string]store.DLPReveal, len(matches))
	if s.BatchReveal != nil {
		unique := make([]string, 0, len(matches))
		seen := make(map[string]struct{}, len(matches))
		for _, g := range matches {
			if _, ok := seen[g[2]]; !ok {
				seen[g[2]] = struct{}{}
				unique = append(unique, g[2])
			}
		}
		batch, err := s.BatchReveal(ctx, workspaceID, unique)
		if err != nil {
			// Fail closed: every pseudonym stays in place, exactly like an
			// unknown token under the per-token path.
			log.Printf("gateway: batched dlp reveal failed: %v", err)
			batch = nil
		}
		revealed = batch
	}
	n := 0
	out := dlpTokenPattern.ReplaceAllStringFunc(content, func(m string) string {
		g := dlpTokenPattern.FindStringSubmatch(m)
		if len(g) != 3 {
			return m
		}
		if s.BatchReveal != nil {
			r, ok := revealed[g[2]]
			if !ok || r.RuleName != g[1] {
				return m
			}
			n++
			return r.Value
		}
		ruleName, value, err := s.Store.RevealDLP(ctx, workspaceID, g[2])
		if err != nil || ruleName != g[1] {
			return m
		}
		n++
		return value
	})
	return out, n
}

// handleDLPReveal implements POST /enterprise/v1/dlp/reveal: batch
// token -> original for the caller's workspace. Accepted credentials: the
// control-plane session (Portal display layer) or an employee API key
// (authorized tooling). Output never includes tokens the caller cannot see.
func (s *Server) handleDLPReveal(w http.ResponseWriter, r *http.Request) {
	auth, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	var body struct {
		Tokens []string `json:"tokens"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeProblem(w, r, err)
		return
	}
	if len(body.Tokens) == 0 || len(body.Tokens) > 50 {
		writeProblem(w, r, errs.New(errs.InvalidArgument, "tokens (1..50) are required"))
		return
	}
	// Subject-aware reveal: a token is restorable when its rule has NO
	// exemptions configured (workspace-member semantics, TASK-028) or the
	// caller's roles are exempt (TASK-029). Anything else misses (fail
	// closed, audited).
	scanner, _ := s.DLP.(*dlp.TableScanner)
	var allRules []dlp.Rule
	if scanner != nil {
		allRules, _ = scanner.AllRules(r.Context())
	}
	exempt := s.subjectExempt(r, session.Workspace.Roles)
	ruleOpen := func(ruleName string) bool {
		if exempt {
			return true
		}
		for _, rule := range allRules {
			if rule.Name == ruleName && len(rule.ExceptRoles) == 0 {
				return true
			}
		}
		return false
	}
	revealed := make(map[string]any, len(body.Tokens))
	missed := 0
	for _, token := range body.Tokens {
		rule, value, err := s.Store.RevealDLP(r.Context(), session.Workspace.WorkspaceID, token)
		if err != nil {
			missed++
			continue
		}
		if !ruleOpen(rule) {
			missed++
			continue
		}
		revealed[token] = map[string]any{"rule": rule, "value": value}
	}
	ev := audit.NewEvent("dlp.reveal", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.Principal.UserID}
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Resource = "dlp_tokens"
	ev.Detail = map[string]any{
		"workspace": session.Workspace.WorkspaceID,
		"requested": len(body.Tokens), "revealed": len(revealed), "missed": missed,
	}
	if err := audit.RequireWrite(r.Context(), s.Store, ev); err != nil {
		writeProblem(w, r, errs.Wrap(err, errs.Internal, "dlp audit delivery failed — reveal refused"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revealed": revealed, "missed": missed})
}
