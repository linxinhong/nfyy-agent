package main

import (
	"context"
	"log"
	"net/http"

	"enterprise-agent-platform/packages/audit"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/governance"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/llmkernel"
	openai "enterprise-agent-platform/packages/protocol-openai"
	"enterprise-agent-platform/packages/token"
	"enterprise-agent-platform/packages/tracing"
)

// handleChatCompletions exposes the approved LLM protocol surface
// (POST /v1/chat/completions). Since TASK-008 the route plan, provider
// endpoints, credential references and pricing are versioned control data in
// PostgreSQL; environment wiring is only a dev fallback (ADR-0008).
//
// Flow: agk auth + workspace membership -> token exchange (aud=llm-gateway,
// sign->verify) -> kernel fail-closed gates -> routed execution with
// deterministic failover -> durable usage/cost row for the selected route.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if s.LLMRouter == nil && s.LLM == nil {
		protocolProblem(w, r, errs.New(errs.NotFound, "llm gateway is not configured"))
		return
	}
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		protocolProblem(w, r, err)
		return
	}
	// Subject-aware DLP context: provider egress (request scan + stream)
	// runs with ForProvider=true so role exemptions NEVER weaken the
	// outbound defense; the response path re-binds the subject per surface
	// below.
	r = withDLPSubject(r, session.Workspace.WorkspaceID, session.Workspace.Roles, true)

	var body openai.ChatCompletionRequest
	if err := decodeJSONBody(w, r, &body); err != nil {
		protocolProblem(w, r, err)
		return
	}
	if body.Model == "" || len(body.Messages) == 0 {
		protocolProblem(w, r, errs.New(errs.InvalidArgument, "model and messages are required"))
		return
	}
	if r, err = s.injectSkills(r, session, &body); err != nil {
		protocolProblem(w, r, err)
		return
	}
	signed, err := s.Signer.Sign(token.IssueInput{
		Session:  session,
		Audience: token.AudienceLLMGateway,
		Now:      s.now(),
	})
	if err != nil {
		protocolProblem(w, r, errs.Wrap(err, errs.Internal, "token exchange failed"))
		return
	}
	claims, err := s.Verifier.Verify(signed, token.AudienceLLMGateway)
	if err != nil {
		protocolProblem(w, r, errs.Wrap(err, errs.Internal, "token exchange verification failed"))
		return
	}
	if body.Stream != nil && *body.Stream {
		// OpenAI streaming is the same public resource with stream=true.  Do
		// not grow a parallel/private endpoint: clients, policy and audit must
		// all observe one contract.
		s.handleChatStream(w, r, session, claims, body)
		return
	}

	chatReq, err := chatRequestFromWire(&body)
	if err != nil {
		protocolProblem(w, r, err)
		return
	}

	// Routed path (TASK-008/010): versioned control data drives the plan and
	// every decision produces reliable, trace-linked audit records.
	if s.LLMRouter != nil {
		var attempts []llmkernel.RouteAttempt
		var trace llmkernel.DecisionTrace
		result, _, _, trace, err := s.LLMRouter.GenerateChatRouted(
			r.Context(), claims, s.Policy, s.DLP, *chatReq,
			func(a llmkernel.RouteAttempt) { attempts = append(attempts, a) },
		)
		// One durable commit for the audit event set + usage ledger row
		// (TASK-041, ADR-0027); any delivery failure withholds the answer.
		if govErr := s.writeGovernanceOutcome(r, session, body.Model, trace, attempts, result, false, err); govErr != nil {
			// Reliable-path failure: the answer is withheld (fail closed).
			log.Printf("gateway: llm governance audit failed: %v", govErr)
			protocolProblem(w, r, errs.New(errs.Internal, "request could not be audited"))
			return
		}
		if err != nil {
			protocolProblem(w, r, err)
			return
		}
		masked, ok := guardChatResult(skillGuardFrom(r.Context()), result)
		s.auditSkillOutput(r, session, masked, !ok)
		if !ok {
			protocolProblem(w, r, errs.New(errs.PermissionDenied, "answer withheld: a tool call copies protected skill text"))
			return
		}
		// Employee-facing surface: exempt roles see originals
		// (re-substitution from the vault); everyone else keeps pseudonyms.
		if s.subjectExempt(r, session.Workspace.Roles) {
			content, n := s.resubstituteTokens(r.Context(), session.Workspace.WorkspaceID, result.Content)
			if n > 0 {
				result.Content = content
			}
		}
		s.writeChatResponse(w, r, result)
		return
	}

	// Legacy single-provider path (dev fallback before control data exists).
	result, err := s.LLM.GenerateChat(r.Context(), claims, s.Policy, s.DLP, s.Usage, *chatReq)
	if err != nil {
		protocolProblem(w, r, err)
		return
	}
	masked, ok := guardChatResult(skillGuardFrom(r.Context()), result)
	s.auditSkillOutput(r, session, masked, !ok)
	if !ok {
		protocolProblem(w, r, errs.New(errs.PermissionDenied, "answer withheld: a tool call copies protected skill text"))
		return
	}
	s.writeChatResponse(w, r, result)
}

func (s *Server) writeChatResponse(w http.ResponseWriter, r *http.Request, result *llmkernel.ChatResult) {
	traceID := tracing.TraceID(r.Context())
	suffix := traceID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	finish := result.FinishReason
	if finish == "" {
		finish = "stop"
	}
	toolCalls := toolCallsToWire(result.ToolCalls)
	msg := openai.Message{Role: "assistant", Content: result.Content, ToolCalls: toolCalls}
	// Provider replies that arrived as content-part arrays are forwarded
	// structurally (ADR-0028 decision 2) — never flattened into text.
	for _, p := range result.ContentParts {
		msg.ContentParts = append(msg.ContentParts, openai.ContentPart{Type: p.Type, Text: p.Text, Raw: p.Raw})
	}
	writeJSON(w, http.StatusOK, openai.ChatCompletionResponse{
		ID:      "chatcmpl-eap-" + suffix,
		Object:  "chat.completion",
		Created: s.now().Unix(),
		Model:   result.Model,
		Choices: []openai.Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: finish,
		}},
		Usage: openai.Usage{
			PromptTokens:     result.Usage.PromptTokens,
			CompletionTokens: result.Usage.CompletionTokens,
			TotalTokens:      result.Usage.TotalTokens,
		},
	})
}

// writeLLMGovernanceAudit delivers the reliable, trace-linked audit event
// set of a routed LLM decision (policy.decision, dlp.hit, llm.request) in
// one durable round trip (TASK-038); any delivery failure still fails
// the request closed at the call site.
func (s *Server) writeLLMGovernanceAudit(r *http.Request, session identity.Session, alias string, trace llmkernel.DecisionTrace, attempts []llmkernel.RouteAttempt, callErr error) error {
	events := s.buildLLMGovernanceEvents(r, session, alias, trace, attempts, callErr)
	return audit.RequireWriteBatch(r.Context(), s.auditSink(), events)
}

// auditSink returns the reliable audit writer: the wired AuditWriter when
// set, the store otherwise (it is an audit.Writer).
func (s *Server) auditSink() audit.Writer {
	if s.AuditWriter != nil {
		return s.AuditWriter
	}
	return s.Store
}

// governanceBatcher is implemented by sinks that can deliver the audit
// event set and the usage ledger row in one transaction (TASK-041, ADR-0027).
type governanceBatcher interface {
	WriteGovernanceBatch(ctx context.Context, events []audit.Event, usage *governance.UsageRecord) error
}

// writeGovernanceOutcome delivers the per-request governance records — the
// reliable audit event set plus, on a successful route, the usage ledger
// row — in one durable commit when the sink supports it; otherwise it falls
// back to the separate audit and usage paths with unchanged semantics. A
// returned error fails the request closed. On a routed error the answer is
// never written and no usage row exists; the audit set carries the outcome.
func (s *Server) writeGovernanceOutcome(r *http.Request, session identity.Session, alias string, trace llmkernel.DecisionTrace, attempts []llmkernel.RouteAttempt, result *llmkernel.ChatResult, streaming bool, callErr error) error {
	events := s.buildLLMGovernanceEvents(r, session, alias, trace, attempts, callErr)

	var usage *governance.UsageRecord
	if result != nil && callErr == nil {
		usage = s.buildUsageRecord(r, session, alias, attempts, result, streaming)
	}

	if gb, ok := s.auditSink().(governanceBatcher); ok {
		return gb.WriteGovernanceBatch(r.Context(), events, usage)
	}
	if err := audit.RequireWriteBatch(r.Context(), s.auditSink(), events); err != nil {
		return err
	}
	if usage != nil {
		s.recordUsage(r, session, alias, attempts, result, streaming)
	}
	return nil
}

// buildLLMGovernanceEvents assembles the three reliable, trace-linked
// records of a routed LLM decision: policy.decision, dlp.hit (when the
// scanner acted) and llm.request. Details carry effects, rule ids,
// redaction counts and route attempts — never raw content or credentials.
func (s *Server) buildLLMGovernanceEvents(r *http.Request, session identity.Session, alias string, trace llmkernel.DecisionTrace, attempts []llmkernel.RouteAttempt, callErr error) []audit.Event {
	base := func(outcome string) audit.Event {
		ev := audit.NewEvent(audit.EventType(outcome), audit.OutcomeAllowed)
		if trace.Outcome == "denied" || callErr != nil {
			ev.Outcome = audit.OutcomeDenied
		}
		ev.Actor = &session.Principal
		ev.WorkspaceID = session.Workspace.WorkspaceID
		ev.RequestID = requestIDOf(r)
		ev.TraceID = traceIDOf(r)
		return ev
	}

	polEv := base("policy.decision")
	polEv.Resource = "llm/" + alias
	polEv.Detail = map[string]any{
		"effect": trace.Effect, "rule_id": trace.RuleID,
		"outcome": trace.Outcome,
	}
	events := []audit.Event{polEv}

	// Alert-mode matches surface here too: rule names present with zero
	// redactions and an allowed outcome = monitored, not enforced.
	if trace.RequestRedactions > 0 || trace.ResponseRedactions > 0 ||
		trace.Outcome == "dlp_blocked" || len(trace.DLPRuleNames) > 0 {
		dlpEv := base("dlp.hit")
		dlpEv.Resource = "llm/" + alias
		dlpEv.Detail = map[string]any{
			"request_redactions":  trace.RequestRedactions,
			"response_redactions": trace.ResponseRedactions,
			"rules":               trace.DLPRuleNames,
			"outcome":             trace.Outcome,
			"alert_only": trace.Outcome != "dlp_blocked" &&
				trace.RequestRedactions == 0 && trace.ResponseRedactions == 0,
		}
		events = append(events, dlpEv)
	}

	llmEv := base("llm.request")
	llmEv.Resource = "llm/" + alias
	llmEv.Detail = map[string]any{
		"effect": trace.Effect, "attempts": len(attempts),
		"request_redactions":  trace.RequestRedactions,
		"response_redactions": trace.ResponseRedactions,
	}
	if callErr != nil {
		if p, ok := errs.From(callErr); ok {
			llmEv.Detail["error_code"] = string(p.Code)
		}
	}
	return append(events, llmEv)
}

// recordUsage writes one durable ledger row for the successful route, linked
// to user, Workspace and trace. Unknown pricing is recorded explicitly with
// cost_known=false — never a fabricated zero-cost success. Ledger failures
// are logged loudly; they do not break the user's answer.
func (s *Server) recordUsage(r *http.Request, session identity.Session, alias string, attempts []llmkernel.RouteAttempt, result *llmkernel.ChatResult, streaming bool) {
	if s.Store == nil || result == nil {
		return
	}
	rec := s.buildUsageRecord(r, session, alias, attempts, result, streaming)
	if rec == nil {
		return
	}
	if err := s.Store.InsertUsage(r.Context(), *rec); err != nil {
		log.Printf("gateway: usage ledger insert failed: %v", err)
	}
}

// buildUsageRecord assembles the ledger row for the selected route; nil
// when no route was selected or pricing could not be resolved.
func (s *Server) buildUsageRecord(r *http.Request, session identity.Session, alias string, attempts []llmkernel.RouteAttempt, result *llmkernel.ChatResult, streaming bool) *governance.UsageRecord {
	var selected llmkernel.RouteAttempt
	for _, a := range attempts {
		if a.Status == "selected" {
			selected = a
			break
		}
	}
	if selected.Provider == "" {
		log.Printf("gateway: usage ledger skipped (no selected route)")
		return nil
	}
	// Pricing comes from the control cache when wired (ADR-0025); the store
	// read is the fallback so every test/dev construction keeps working.
	pricingLookup := s.Pricing
	if pricingLookup == nil {
		pricingLookup = s.Store.PricingKnown
	}
	pricing, known, version, err := pricingLookup(r.Context(), alias, selected.Provider)
	if err != nil {
		log.Printf("gateway: usage pricing lookup failed: %v", err)
		return nil
	}
	if !known {
		log.Printf("gateway: USAGE PRICING UNKNOWN for %s@%s — recorded with cost_known=false", alias, selected.Provider)
	}
	return &governance.UsageRecord{
		TraceID:          tracing.TraceID(r.Context()),
		RequestID:        requestIDOf(r),
		Attempt:          len(attempts),
		UserID:           session.Principal.UserID,
		WorkspaceID:      session.Workspace.WorkspaceID,
		Alias:            alias,
		Provider:         selected.Provider,
		ProviderModel:    selected.Model,
		PromptTokens:     result.Usage.PromptTokens,
		CompletionTokens: result.Usage.CompletionTokens,
		TotalTokens:      result.Usage.TotalTokens,
		Streaming:        streaming,
		Pricing:          pricing,
		PricingVer:       version,
		PricingKnown:     known,
	}
}

// chatRequestFromWire converts the wire request into the kernel request
// (ADR-0025): tools and tool_choice travel as opaque JSON, tool-calling
// message fields are mapped explicitly.
func chatRequestFromWire(body *openai.ChatCompletionRequest) (*llmkernel.ChatRequest, error) {
	req := &llmkernel.ChatRequest{
		Model: body.Model, Tools: body.Tools, ToolChoice: body.ToolChoice,
		Temperature: body.Temperature, TopP: body.TopP, MaxTokens: body.MaxTokens,
		Stop: body.Stop, ParallelToolCalls: body.ParallelToolCalls, StreamOptions: body.StreamOptions,
		Extra: body.Extra,
	}
	for _, m := range body.Messages {
		km := llmkernel.ChatMessage{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
		// Content parts keep their original structure toward the provider
		// (ADR-0028 decision 2); m.Content stays the flattened text that
		// drives DLP.
		for _, p := range m.ContentParts {
			km.ContentParts = append(km.ContentParts, llmkernel.ContentPart{Type: p.Type, Text: p.Text, Raw: p.Raw})
		}
		for _, tc := range m.ToolCalls {
			km.ToolCalls = append(km.ToolCalls, llmkernel.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		req.Messages = append(req.Messages, km)
	}
	return req, nil
}

// toolCallsToWire maps kernel tool calls back onto the OpenAI wire shape.
func toolCallsToWire(tcs []llmkernel.ToolCall) []openai.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]openai.ToolCall, 0, len(tcs))
	for _, tc := range tcs {
		out = append(out, openai.ToolCall{
			ID:       tc.ID,
			Type:     "function",
			Function: openai.ToolCallFunction{Name: tc.Name, Arguments: tc.Arguments},
		})
	}
	return out
}
