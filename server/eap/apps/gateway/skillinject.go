package main

// Protected skill injection on /v1/chat/completions (packages/skillshare,
// packages/skillguard).
//
// Why here: a protected SKILL.md must reach the model without ever reaching
// the member's machine. The client only names the skills it activated
// (X-MindLink-Skills); the gateway checks each is active and visible for the
// caller, splices the text in as a system message after the client's own
// system prompt (so the cached prefix stays stable while the skill set does),
// and masks copied runs on the way back.
//
// Invariants and pitfalls:
//   - The injected text rides the normal request path, so outbound Policy and
//     DLP see it like any other prompt text; audit records which skills were
//     injected, never their text.
//   - A name the caller may not use is skipped, not an error: a stale client
//     list must not break the conversation, and the response never says why.
//   - Tool-call arguments cannot be masked without breaking the JSON a tool
//     expects, so a tool call that copies skill text withholds the answer.
//   - Only chat completions serve skills; the other inference routes refuse
//     the header rather than silently answer without the skill.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/llmkernel"
	openai "enterprise-agent-platform/packages/protocol-openai"
	"enterprise-agent-platform/packages/skillguard"
	"enterprise-agent-platform/packages/skillshare"
)

// HeaderSkills names the skills the client activated, comma-separated.
const HeaderSkills = "X-MindLink-Skills"

const maxInjectedSkills = 16

var skillNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

type skillGuardKey struct{}

func skillGuardFrom(ctx context.Context) *skillguard.Guard {
	g, _ := ctx.Value(skillGuardKey{}).(*skillguard.Guard)
	return g
}

func requestedSkills(r *http.Request) ([]string, error) {
	raw := strings.TrimSpace(r.Header.Get(HeaderSkills))
	if raw == "" {
		return nil, nil
	}
	var names []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if !skillNamePattern.MatchString(name) {
			return nil, errs.New(errs.InvalidArgument, HeaderSkills+" carries an invalid skill name")
		}
		names = append(names, name)
	}
	if len(names) > maxInjectedSkills {
		return nil, errs.New(errs.InvalidArgument, fmt.Sprintf("at most %d skills per request", maxInjectedSkills))
	}
	return names, nil
}

// refuseSkillHeader answers routes that do not serve protected skills.
func refuseSkillHeader(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get(HeaderSkills)) == "" {
		return false
	}
	protocolProblem(w, r, errs.New(errs.InvalidArgument, "protected skills are served on /v1/chat/completions only"))
	return true
}

// injectSkills splices the caller's active protected skills into body and
// returns the request carrying their output guard.
func (s *Server) injectSkills(r *http.Request, session identity.Session, body *openai.ChatCompletionRequest) (*http.Request, error) {
	names, err := requestedSkills(r)
	if err != nil || len(names) == 0 {
		return r, err
	}
	if s.Skills == nil {
		return r, errs.New(errs.InvalidArgument, "skill sharing is not enabled on this gateway")
	}
	grants, err := s.grantsFor(r.Context(), session.Workspace.WorkspaceID, session.Principal.UserID, session.Workspace.Roles)
	if err != nil {
		return r, errs.Wrap(err, errs.Internal, "permission lookup failed")
	}
	loaded, skipped, err := s.Skills.Bodies(r.Context(), session.Workspace.WorkspaceID,
		skillshare.Viewer{UserID: session.Principal.UserID, Grants: grants}, names)
	if err != nil {
		return r, errs.Wrap(err, errs.Internal, "skill lookup failed")
	}
	ev := audit.NewEvent("skill.injected", audit.OutcomeAllowed)
	ev.Actor = &session.Principal
	ev.WorkspaceID = session.Workspace.WorkspaceID
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	injected := make([]string, 0, len(loaded))
	for _, l := range loaded {
		injected = append(injected, l.Name+"@"+l.Version)
	}
	ev.Detail = map[string]any{"skills": injected, "skipped": skipped}
	if err := audit.RequireWrite(r.Context(), s.auditSink(), ev); err != nil {
		return r, errs.Wrap(err, errs.Internal, "skill injection could not be audited")
	}
	if len(loaded) == 0 {
		return r, nil
	}
	var block strings.Builder
	block.WriteString("<mindlink-protected-skills>\n")
	block.WriteString(skillguard.Instructions)
	texts := make([]string, 0, len(loaded))
	for _, l := range loaded {
		fmt.Fprintf(&block, "\n\n<skill name=%q version=%q>\n%s\n</skill>", l.Name, l.Version, strings.TrimSpace(l.Body))
		texts = append(texts, l.Body)
	}
	block.WriteString("\n</mindlink-protected-skills>")
	at := 0
	for at < len(body.Messages) && body.Messages[at].Role == "system" {
		at++
	}
	msgs := make([]openai.Message, 0, len(body.Messages)+1)
	msgs = append(msgs, body.Messages[:at]...)
	msgs = append(msgs, openai.Message{Role: "system", Content: block.String()})
	body.Messages = append(msgs, body.Messages[at:]...)
	guard := skillguard.New(texts...)
	if guard == nil {
		return r, nil
	}
	return r.WithContext(context.WithValue(r.Context(), skillGuardKey{}, guard)), nil
}

// guardChatResult masks copied skill text in a complete answer; a tool call
// that copies it withholds the answer (false).
func guardChatResult(guard *skillguard.Guard, result *llmkernel.ChatResult) (masked int, ok bool) {
	if guard == nil {
		return 0, true
	}
	for _, tc := range result.ToolCalls {
		if guard.Leaks(tc.Arguments) {
			return 0, false
		}
	}
	result.Content, masked = guard.Redact(result.Content)
	for i, p := range result.ContentParts {
		if p.Type != "text" {
			continue
		}
		text, n := guard.Redact(p.Text)
		if n == 0 {
			continue
		}
		masked += n
		raw, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		result.ContentParts[i].Text, result.ContentParts[i].Raw = text, raw
	}
	return masked, true
}

// auditSkillOutput records masked or withheld output; best effort after the
// answer is decided (the injection itself was audited reliably).
func (s *Server) auditSkillOutput(r *http.Request, session identity.Session, masked int, withheld bool) {
	if masked == 0 && !withheld {
		return
	}
	ev := audit.NewEvent("skill.output_guarded", audit.OutcomeAllowed)
	if withheld {
		ev.Outcome = audit.OutcomeDenied
	}
	ev.Actor = &session.Principal
	ev.WorkspaceID = session.Workspace.WorkspaceID
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	ev.Detail = map[string]any{"masked_runs": masked, "withheld": withheld}
	if err := audit.RequireWrite(r.Context(), s.auditSink(), ev); err != nil {
		log.Printf("gateway: skill output audit failed: %v", err)
	}
}
