package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"

	"enterprise-agent-platform/packages/llmkernel"
	openai "enterprise-agent-platform/packages/protocol-openai"
	"enterprise-agent-platform/packages/skillguard"
	"enterprise-agent-platform/packages/token"
	"enterprise-agent-platform/packages/tracing"
)

// openAIError writes an OpenAI-shaped error body (protocol compatibility).
// The frozen EAP code rides error.code, the taxonomy type rides error.type,
// and the trace id stays observable as error.trace_id.
func openAIError(w http.ResponseWriter, status int, code string, message, traceID string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":  message,
			"type":     openAIErrorType(errs.Code(code)),
			"code":     code,
			"param":    nil,
			"trace_id": traceID,
		},
	})
}

// openAIErrorType maps frozen EAP codes onto the OpenAI error taxonomy
// (openai/openai-openai): clients branch on type for retry/auth handling.
func openAIErrorType(code errs.Code) string {
	switch code {
	case errs.InvalidArgument, errs.NotFound, errs.Conflict:
		return "invalid_request_error"
	case errs.Unauthenticated:
		return "authentication_error"
	case errs.PermissionDenied, errs.WorkspaceForbidden, errs.PolicyDenied, errs.DLPBlocked:
		return "permission_error"
	case errs.RateLimited, errs.QuotaExceeded:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

func protocolProblem(w http.ResponseWriter, r *http.Request, err error) {
	problem, ok := errs.From(err)
	if !ok {
		problem = errs.Problem{Code: errs.Internal, Message: "internal error"}
	}
	traceID := ""
	if meta, ok := tracing.FromContext(r.Context()); ok {
		problem.TraceID = meta.Trace.TraceID
		traceID = meta.Trace.TraceID
	}
	openAIError(w, problem.Code.HTTPStatus(), string(problem.Code), problem.Message, traceID)
}

// commonProtocolAuth: agk key + workspace membership + token exchange to
// aud=llm-gateway (sign->verify). Shared by every protocol endpoint.
func (s *Server) commonProtocolAuth(w http.ResponseWriter, r *http.Request) (identity.Session, *token.Claims, bool) {
	if s.LLMRouter == nil {
		protocolProblem(w, r, errs.New(errs.NotFound, "llm gateway is not configured"))
		return identity.Session{}, nil, false
	}
	auth, ok := s.requireAPIKey(w, r)
	if !ok {
		return identity.Session{}, nil, false
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		protocolProblem(w, r, err)
		return identity.Session{}, nil, false
	}
	signed, err := s.Signer.Sign(token.IssueInput{Session: session, Audience: token.AudienceLLMGateway, Now: s.now()})
	if err != nil {
		protocolProblem(w, r, errs.Wrap(err, errs.Internal, "token exchange failed"))
		return identity.Session{}, nil, false
	}
	claims, err := s.Verifier.Verify(signed, token.AudienceLLMGateway)
	if err != nil {
		protocolProblem(w, r, errs.Wrap(err, errs.Internal, "token exchange verification failed"))
		return identity.Session{}, nil, false
	}
	return session, claims, true
}

// handleModels implements GET /v1/models: logical aliases from control data
// that the caller's workspace may use (policy-filtered; unknown = hidden).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if s.LLMRouter == nil {
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
	aliases, err := s.Store.ListEnabledAliases(r.Context())
	if err != nil {
		protocolProblem(w, r, err)
		return
	}
	data := make([]openai.Model, 0, len(aliases))
	for _, alias := range aliases {
		decision, err := s.Policy.Decide(r.Context(), llmkernel.PolicyInput{
			Session: gatewayClaimSession{userID: session.Principal.UserID, workspace: session.Workspace.WorkspaceID},
			Model:   alias, Action: "llm.models",
		})
		if err != nil {
			protocolProblem(w, r, errs.PolicyFailClosed(err.Error()))
			return
		}
		switch decision.Effect {
		case llmkernel.EffectDeny, llmkernel.EffectRequireApproval:
			continue // hidden from discovery, consistent with execution
		}
		data = append(data, openai.Model{ID: alias, Object: "model", Created: s.now().Unix(), OwnedBy: "eap"})
	}
	writeJSON(w, http.StatusOK, openai.ModelsResponse{Object: "list", Data: data})
}

// handleChatStream implements POST /v1/chat/completions with stream=true:
// the same gates (policy/DLP run once), routed execution, SSE frames in the
// frozen chunk shape; client cancellation tears down the upstream stream.
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request, session identity.Session, claims *token.Claims, body openai.ChatCompletionRequest) {
	chatReq, err := chatRequestFromWire(&body)
	if err != nil {
		protocolProblem(w, r, err)
		return
	}
	var attempts []llmkernel.RouteAttempt
	// result carries only the model; content/usage accumulate from chunks
	// (single-reader contract — this handler is the only chunk consumer).
	result, chunks, errCh, trace, err := s.LLMRouter.GenerateChatStreamRouted(
		r.Context(), claims, s.Policy, s.DLP, *chatReq,
		func(a llmkernel.RouteAttempt) { attempts = append(attempts, a) },
	)
	if err != nil {
		s.writeLLMGovernanceAudit(r, session, body.Model, trace, attempts, err)
		protocolProblem(w, r, err)
		return
	}
	result.Content = ""
	finishReason := ""

	flusher, ok := w.(http.Flusher)
	if !ok {
		protocolProblem(w, r, errs.New(errs.Internal, "streaming is not supported by this connection"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	traceID := tracing.TraceID(r.Context())
	suffix := traceID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	id := "chatcmpl-eap-" + suffix
	created := s.now().Unix()
	emit := func(chunk openai.ChatCompletionChunk) {
		b, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		flusher.Flush()
	}

	var content strings.Builder
	// Protected skills ride the same window: copied runs are masked before
	// release, so the window must hold a whole guard run.
	guard := skillGuardFrom(r.Context())
	holdback, masked := s.dlpMaxMatch(r), 0
	if guard != nil && holdback < skillguard.HoldbackBytes {
		holdback = skillguard.HoldbackBytes
	}
	// Cross-chunk DLP window: release only prefixes that can no longer match
	// any rule; deny aborts; tokenize/redact rewrite (TASK-030).
	san := newSSESanitizer(func(text string) (llmkernel.DLPResult, error) {
		res := llmkernel.DLPResult{Redacted: text}
		if s.DLP != nil {
			var err error
			if res, err = s.DLP.Scan(r.Context(), text); err != nil || res.Blocked {
				return res, err
			}
		}
		if guard != nil {
			var n int
			res.Redacted, n = guard.Redact(res.Redacted)
			masked += n
		}
		return res, nil
	}, holdback)
	// Tool-call argument fragments cannot be masked without breaking their
	// JSON: with skills injected they are held until the end and released
	// only if they copy none of the skill text.
	var heldTools []openai.ToolCall
	releaseTools := func() bool {
		for _, tc := range heldTools {
			if guard.Leaks(tc.Function.Arguments) {
				s.auditSkillOutput(r, session, masked, true)
				emit(openai.ChatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: result.Model,
					Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: "[withheld:protected-skill]"}, FinishReason: strPtr("error")}},
				})
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				flusher.Flush()
				return false
			}
		}
		for _, tc := range heldTools {
			emit(openai.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: result.Model,
				Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{ToolCalls: []openai.ToolCall{tc}}}},
			})
		}
		s.auditSkillOutput(r, session, masked, false)
		return true
	}
	// Exempt roles see originals in the stream too: released text (and the
	// final tail) has [rule:token] pseudonyms restored from the vault. The
	// scan itself stays provider-grade — re-substitution only touches what
	// leaves toward the employee.
	if s.subjectExempt(r, session.Workspace.Roles) {
		san.release = func(text string) string {
			out, _ := s.resubstituteTokens(r.Context(), session.Workspace.WorkspaceID, text)
			return out
		}
	}
	emitDelta := func(delta string) {
		if delta == "" {
			return
		}
		release, aborted, err := san.Push(delta)
		if err != nil {
			release, aborted = delta, false
		}
		if aborted {
			emit(openai.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: result.Model,
				Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: "[withheld:dlp]"}, FinishReason: strPtr("error")}},
			})
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
		if release != "" {
			emit(openai.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: result.Model,
				Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: release}}},
			})
		}
	}
	flushTail := func() {
		tail, aborted, err := san.Flush()
		if err == nil && aborted {
			return
		}
		if err == nil && tail != "" {
			emit(openai.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: result.Model,
				Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: tail}}},
			})
		}
	}
	for {
		select {
		case <-r.Context().Done():
			// Client cancellation: the router's stream ctx is a child of the
			// request ctx, so upstream work is torn down with it.
			result.Content = content.String()
			flushTail()
			s.writeGovernanceOutcome(r, session, body.Model, trace, attempts, result, true, nil)
			return
		case chunk, ok := <-chunks:
			if !ok {
				// TASK-055: the router's stream goroutine delivers a
				// mid-stream death on the buffered errCh and then closes BOTH
				// channels, so a closed chunks channel alone proves nothing.
				// Prefer the buffered death error over masquerading as clean
				// completion (Go's select picks randomly when both are ready).
				if streamErr, pending := pendingStreamError(errCh); pending {
					s.writeLLMGovernanceAudit(r, session, body.Model, trace, attempts, streamErr)
					// Mid-stream failures surface as a terminal SSE error event.
					emit(openai.ChatCompletionChunk{
						ID: id, Object: "chat.completion.chunk", Created: created, Model: body.Model,
						Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: "[stream error]"}, FinishReason: strPtr("error")}},
					})
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
					flusher.Flush()
					return
				}
				result.Content = content.String()
				releaseTools()
				s.writeGovernanceOutcome(r, session, body.Model, trace, attempts, result, true, nil)
				return
			}
			if chunk.Done {
				applyStreamUsage(result, chunk)
				result.Content = content.String()
				flushTail()
				if !releaseTools() {
					s.writeGovernanceOutcome(r, session, body.Model, trace, attempts, result, true, nil)
					return
				}
				if finishReason == "" {
					finishReason = "stop"
				}
				emit(openai.ChatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: result.Model,
					Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{}, FinishReason: strPtr(finishReason)}},
				})
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				flusher.Flush()
				s.writeGovernanceOutcome(r, session, body.Model, trace, attempts, result, true, nil)
				return
			}
			applyStreamUsage(result, chunk)
			content.WriteString(chunk.DeltaContent)
			if chunk.FinishReason != "" {
				finishReason = chunk.FinishReason
			}
			// Non-text parts of a provider delta are forwarded structurally
			// (ADR-0028 decision 2) and never enter the SSE text sanitizer;
			// text parts ride the governed text path below.
			if nt := nonTextWireParts(chunk.DeltaParts); len(nt) > 0 {
				emit(openai.ChatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: result.Model,
					Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{ContentParts: nt}}},
				})
			}
			emitDelta(chunk.DeltaContent)
			// Tool_calls deltas are JSON argument fragments (ADR-0025): they
			// bypass the content sanitizer, which must never text-transform
			// them.
			for _, tc := range chunk.ToolCalls {
				idx := tc.Index
				delta := openai.ToolCall{Index: &idx, Type: "function", Function: openai.ToolCallFunction{Name: tc.Name, Arguments: tc.Arguments}}
				if tc.ID != "" {
					delta.ID = tc.ID
				}
				if guard != nil {
					heldTools = mergeToolDelta(heldTools, delta)
					continue
				}
				emit(openai.ChatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: result.Model,
					Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{ToolCalls: []openai.ToolCall{delta}}}},
				})
			}
		case err, open := <-errCh:
			if !open {
				errCh = nil
				continue
			}
			s.writeLLMGovernanceAudit(r, session, body.Model, trace, attempts, err)
			// Mid-stream failures surface as a terminal SSE error event.
			emit(openai.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: body.Model,
				Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: "[stream error]"}, FinishReason: strPtr("error")}},
			})
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
	}
}

// sseHoldbackCap bounds the sanitizer window when the rule set contains a
// pattern whose match length cannot be bounded (documented cap, TASK-039);
// finite rule sets always derive their exact bound instead.
const sseHoldbackCap = 4096

// dlpMaxMatch derives the longest possible rule match from the live rule
// set so no match can straddle an SSE release boundary. Non-table engines
// keep the historical 64-byte bound.
func (s *Server) dlpMaxMatch(r *http.Request) int {
	scanner, ok := s.DLP.(*dlp.TableScanner)
	if !ok {
		return 64
	}
	m, bounded, err := scanner.MaxMatchBytes(r.Context())
	if err != nil || !bounded || m > sseHoldbackCap {
		return sseHoldbackCap
	}
	return m
}

func strPtr(s string) *string { return &s }

// pendingStreamError performs the non-blocking errCh drain that guards the
// closed-chunks completion path in every streaming handler (TASK-055): the
// router's stream goroutine delivers a mid-stream death on the BUFFERED errCh
// and only then closes both channels, so once chunks is observed closed any
// death error is already queued behind the close. Without this drain, Go's
// random select pick between the two ready cases could turn a provider death
// into a success: no terminal error frame, an allowed audit outcome and a
// fabricated zero-usage ledger row. A nil (already drained) errCh never
// selects ready, so the no-error path stays on clean completion.
func pendingStreamError(errCh <-chan error) (error, bool) {
	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			return err, true
		}
	default:
	}
	return nil, false
}

// nonTextWireParts strips text parts (which stay on the governed text path)
// from a provider delta and maps the remaining parts onto the wire shape.
func nonTextWireParts(parts []llmkernel.ContentPart) []openai.ContentPart {
	var out []openai.ContentPart
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			continue
		}
		out = append(out, openai.ContentPart{Type: p.Type, Text: p.Text, Raw: p.Raw})
	}
	return out
}

// applyStreamUsage keeps the last provider usage frame. OpenAI providers
// commonly put usage on a preceding chunk and follow it with a bare [DONE],
// so a terminal marker with zero fields must not erase real accounting.
func applyStreamUsage(result *llmkernel.ChatResult, chunk llmkernel.StreamChunk) {
	if result == nil || (chunk.PromptTokens == 0 && chunk.CompletionTokens == 0 && chunk.TotalTokens == 0) {
		return
	}
	result.Usage = llmkernel.Usage{PromptTokens: chunk.PromptTokens, CompletionTokens: chunk.CompletionTokens, TotalTokens: chunk.TotalTokens}
}

// openSSE sets the common streaming response boundary only after the routed
// security checks have accepted the request. Errors before this point retain
// the normal JSON protocol error shape.
func openSSE(w http.ResponseWriter) (func(string, any), bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return func(event string, value any) {
		if event != "" {
			_, _ = w.Write([]byte("event: " + event + "\n"))
		}
		if value != nil {
			encoded, _ := json.Marshal(value)
			_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
		} else {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
		flusher.Flush()
	}, true
}

// handleResponsesStream maps the routed chat stream to the documented
// Responses event vocabulary. It deliberately uses the same router as Chat:
// no protocol can bypass workspace selection, policy, DLP, audit or cancel.
func (s *Server) handleResponsesStream(w http.ResponseWriter, r *http.Request, session identity.Session, claims *token.Claims, model, input string) {
	var attempts []llmkernel.RouteAttempt
	result, chunks, errCh, trace, err := s.LLMRouter.GenerateChatStreamRouted(r.Context(), claims, s.Policy, s.DLP,
		llmkernel.ChatRequest{Model: model, Messages: []llmkernel.ChatMessage{{Role: "user", Content: input}}},
		func(a llmkernel.RouteAttempt) { attempts = append(attempts, a) })
	if err != nil {
		_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, err)
		protocolProblem(w, r, err)
		return
	}
	emit, ok := openSSE(w)
	if !ok {
		protocolProblem(w, r, errs.New(errs.Internal, "streaming is not supported by this connection"))
		return
	}
	id := "resp_eap_" + shortTrace(r)
	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": result.Model}})
	var content strings.Builder
	for {
		select {
		case <-r.Context().Done():
			result.Content = content.String()
			s.recordUsage(r, session, model, attempts, result, true)
			_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, nil)
			return
		case chunk, alive := <-chunks:
			if !alive {
				// TASK-055: the router closes BOTH channels on a mid-stream
				// death (the error is buffered on errCh first) — drain it
				// before reading closed chunks as clean completion.
				if streamErr, pending := pendingStreamError(errCh); pending {
					_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, streamErr)
					emit("error", map[string]any{"type": "error", "error": map[string]string{"code": "upstream_unavailable", "message": "stream interrupted"}})
					return
				}
			}
			if !alive || chunk.Done {
				if alive {
					applyStreamUsage(result, chunk)
				}
				result.Content = content.String()
				emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "response_id": id, "output_index": 0, "content_index": 0, "text": result.Content})
				emit("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "status": "completed", "model": result.Model}})
				s.recordUsage(r, session, model, attempts, result, true)
				_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, nil)
				return
			}
			applyStreamUsage(result, chunk)
			content.WriteString(chunk.DeltaContent)
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "response_id": id, "output_index": 0, "content_index": 0, "delta": chunk.DeltaContent})
		case streamErr, open := <-errCh:
			if !open {
				errCh = nil
				continue
			}
			_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, streamErr)
			emit("error", map[string]any{"type": "error", "error": map[string]string{"code": "upstream_unavailable", "message": "stream interrupted"}})
			return
		}
	}
}

// handleMessagesStream translates the same routed stream to Anthropic's
// event sequence. The message endpoint remains compatible for clients that
// choose either JSON or SSE by toggling stream.
func (s *Server) handleMessagesStream(w http.ResponseWriter, r *http.Request, session identity.Session, claims *token.Claims, model string, messages []llmkernel.ChatMessage) {
	var attempts []llmkernel.RouteAttempt
	result, chunks, errCh, trace, err := s.LLMRouter.GenerateChatStreamRouted(r.Context(), claims, s.Policy, s.DLP,
		llmkernel.ChatRequest{Model: model, Messages: messages},
		func(a llmkernel.RouteAttempt) { attempts = append(attempts, a) })
	if err != nil {
		_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, err)
		protocolProblem(w, r, err)
		return
	}
	emit, ok := openSSE(w)
	if !ok {
		protocolProblem(w, r, errs.New(errs.Internal, "streaming is not supported by this connection"))
		return
	}
	id := "msg_eap_" + shortTrace(r)
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": result.Model, "content": []any{}, "stop_reason": nil}})
	emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
	var content strings.Builder
	for {
		select {
		case <-r.Context().Done():
			result.Content = content.String()
			s.recordUsage(r, session, model, attempts, result, true)
			_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, nil)
			return
		case chunk, alive := <-chunks:
			if !alive {
				// TASK-055: the router closes BOTH channels on a mid-stream
				// death (the error is buffered on errCh first) — drain it
				// before reading closed chunks as clean completion.
				if streamErr, pending := pendingStreamError(errCh); pending {
					_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, streamErr)
					emit("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "stream interrupted"}})
					return
				}
			}
			if !alive || chunk.Done {
				if alive {
					applyStreamUsage(result, chunk)
				}
				result.Content = content.String()
				emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
				emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": result.Usage.CompletionTokens}})
				emit("message_stop", map[string]string{"type": "message_stop"})
				s.recordUsage(r, session, model, attempts, result, true)
				_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, nil)
				return
			}
			applyStreamUsage(result, chunk)
			content.WriteString(chunk.DeltaContent)
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": chunk.DeltaContent}})
		case streamErr, open := <-errCh:
			if !open {
				errCh = nil
				continue
			}
			_ = s.writeLLMGovernanceAudit(r, session, model, trace, attempts, streamErr)
			emit("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "stream interrupted"}})
			return
		}
	}
}

// handleResponses implements POST /v1/responses (OpenAI Responses shape,
// non-streaming): input text in, output text out, through the same routed
// pipeline.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if refuseSkillHeader(w, r) {
		return
	}
	session, claims, ok := s.commonProtocolAuth(w, r)
	if !ok {
		return
	}
	var body struct {
		Model  string `json:"model"`
		Input  string `json:"input"`
		Stream bool   `json:"stream"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		protocolProblem(w, r, err)
		return
	}
	if body.Model == "" || body.Input == "" {
		protocolProblem(w, r, errs.New(errs.InvalidArgument, "model and input are required"))
		return
	}
	if body.Stream {
		s.handleResponsesStream(w, r, session, claims, body.Model, body.Input)
		return
	}
	result, _, _, _, err := s.LLMRouter.GenerateChatRouted(r.Context(), claims, s.Policy, s.DLP,
		llmkernel.ChatRequest{Model: body.Model, Messages: []llmkernel.ChatMessage{{Role: "user", Content: body.Input}}},
		nil)
	if err != nil {
		protocolProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     "resp_eap_" + shortTrace(r),
		"object": "response",
		"model":  result.Model,
		"output": []map[string]any{{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": result.Content}},
		}},
		"usage": map[string]any{
			"input_tokens": result.Usage.PromptTokens, "output_tokens": result.Usage.CompletionTokens,
			"total_tokens": result.Usage.TotalTokens,
		},
	})
}

// handleEmbeddings implements POST /v1/embeddings through the primary route
// provider with the same credential boundary.
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.commonProtocolAuth(w, r)
	if !ok {
		return
	}
	var body struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		protocolProblem(w, r, err)
		return
	}
	if body.Model == "" || body.Input == "" {
		protocolProblem(w, r, errs.New(errs.InvalidArgument, "model and input are required"))
		return
	}
	plan, err := s.Store.RoutePlanFor(r.Context(), body.Model)
	if err != nil {
		protocolProblem(w, r, err)
		return
	}
	route := plan.Routes[0]
	target, err := s.Store.ResolveProviderTarget(r.Context(), route.Provider)
	if err != nil {
		protocolProblem(w, r, err)
		return
	}
	credential, err := s.Credential(r.Context(), target.CredentialRef)
	if err != nil {
		protocolProblem(w, r, errs.Wrap(err, errs.Internal, "credential resolution failed"))
		return
	}
	vector, err := (&llmkernel.OpenAICompatClient{}).Embeddings(r.Context(), target, credential, route.Model, body.Input)
	if err != nil {
		protocolProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []map[string]any{{"object": "embedding", "index": 0, "embedding": vector}},
		"model":  body.Model,
		"usage":  map[string]any{"prompt_tokens": 0, "total_tokens": 0},
	})
}

// handleMessages implements POST /v1/messages (Anthropic Messages compat):
// translate to the routed chat pipeline, respond in Anthropic shape.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if refuseSkillHeader(w, r) {
		return
	}
	session, claims, ok := s.commonProtocolAuth(w, r)
	if !ok {
		return
	}
	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Stream    bool   `json:"stream"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		protocolProblem(w, r, err)
		return
	}
	if body.Model == "" || len(body.Messages) == 0 {
		protocolProblem(w, r, errs.New(errs.InvalidArgument, "model and messages are required"))
		return
	}
	messages := make([]llmkernel.ChatMessage, 0, len(body.Messages)+1)
	if body.System != "" {
		messages = append(messages, llmkernel.ChatMessage{Role: "system", Content: body.System})
	}
	for _, m := range body.Messages {
		messages = append(messages, llmkernel.ChatMessage{Role: m.Role, Content: m.Content})
	}
	if body.Stream {
		s.handleMessagesStream(w, r, session, claims, body.Model, messages)
		return
	}
	result, _, _, _, err := s.LLMRouter.GenerateChatRouted(r.Context(), claims, s.Policy, s.DLP,
		llmkernel.ChatRequest{Model: body.Model, Messages: messages}, nil)
	if err != nil {
		protocolProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "msg_eap_" + shortTrace(r), "type": "message", "role": "assistant",
		"model":       result.Model,
		"content":     []map[string]any{{"type": "text", "text": result.Content}},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": result.Usage.PromptTokens, "output_tokens": result.Usage.CompletionTokens},
	})
}

func shortTrace(r *http.Request) string {
	id := tracing.TraceID(r.Context())
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// gatewayClaimSession adapts a session to the kernel's minimal identity seam
// (mirrors the llmkernel-internal claimSession without importing internals).
type gatewayClaimSession struct {
	userID    string
	workspace string
}

func (g gatewayClaimSession) SessionUserID() string      { return g.userID }
func (g gatewayClaimSession) SessionWorkspaceID() string { return g.workspace }

// mergeToolDelta folds one streamed tool-call fragment into the held calls
// (same index: name/id set once, arguments appended).
func mergeToolDelta(held []openai.ToolCall, delta openai.ToolCall) []openai.ToolCall {
	for i := range held {
		if *held[i].Index == *delta.Index {
			if delta.ID != "" {
				held[i].ID = delta.ID
			}
			if delta.Function.Name != "" {
				held[i].Function.Name = delta.Function.Name
			}
			held[i].Function.Arguments += delta.Function.Arguments
			return held
		}
	}
	return append(held, delta)
}
