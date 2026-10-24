package llmkernel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/token"
)

// StreamChunk is one provider-side streaming delta.
type StreamChunk struct {
	DeltaContent string
	// DeltaParts preserves provider deltas that arrived as content-part
	// arrays (ADR-0028 decision 2): non-text parts must be forwarded
	// structurally and never enter the SSE text sanitizer.
	DeltaParts       []ContentPart
	ToolCalls        []ToolCall // partial tool_calls deltas (ADR-0025)
	FinishReason     string
	Done             bool
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// OpenAICompatClient streaming: returns a channel of deltas. Cancelling ctx
// tears down the upstream connection (our own HTTP client — the pinned
// Bifrost stream-cancellation race is not on this path).
func (c *OpenAICompatClient) ChatStream(ctx context.Context, target ProviderTarget, credential string, req ChatRequest) (<-chan StreamChunk, <-chan error) {
	chunks := make(chan StreamChunk, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(chunks)
		defer close(errCh)
		payload := openAIChatPayload{
			Model: req.Model, Stream: true, Tools: req.Tools, ToolChoice: req.ToolChoice,
			Temperature: req.Temperature, TopP: req.TopP, MaxTokens: req.MaxTokens,
			Stop: req.Stop, ParallelToolCalls: req.ParallelToolCalls, StreamOptions: req.StreamOptions,
		}
		for _, m := range req.Messages {
			content := any(m.Content)
			if parts := contentPartsWire(m.ContentParts); parts != nil {
				content = parts
			}
			payload.Messages = append(payload.Messages, openAIMessageIn{
				Role: m.Role, Content: content, Name: m.Name,
				ToolCalls: toolCallsToWire(m.ToolCalls), ToolCallID: m.ToolCallID,
			})
		}
		body, err := json.Marshal(payload)
		if err != nil {
			errCh <- errs.Wrap(err, errs.Internal, "stream payload build failed")
			return
		}
		endpoint := strings.TrimSuffix(target.BaseURL, "/") + "/chat/completions"
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			errCh <- errs.Wrap(err, errs.Internal, "stream request build failed")
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if credential != "" {
			httpReq.Header.Set("Authorization", "Bearer "+credential)
		}
		maxBytes := c.MaxResponseBytes
		if maxBytes == 0 {
			maxBytes = 8 << 20
		}
		resp, err := c.httpClient().Do(httpReq)
		if err != nil {
			errCh <- errs.Wrap(err, errs.UpstreamUnavailable, "provider stream unreachable").AsRetriable()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			errCh <- errs.Wrap(fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(raw), 200)), errs.UpstreamUnavailable, "provider stream rejected")
			return
		}
		scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxBytes))
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if ctx.Err() != nil {
				errCh <- ctx.Err()
				return
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				chunks <- StreamChunk{Done: true}
				return
			}
			var frame struct {
				Choices []struct {
					Delta struct {
						Content   json.RawMessage  `json:"content"`
						ToolCalls []openAIToolCall `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				continue // keep-alives or non-JSON frames are skipped
			}
			chunk := StreamChunk{}
			if len(frame.Choices) > 0 {
				chunk.DeltaContent, chunk.DeltaParts = decodeReplyContent(frame.Choices[0].Delta.Content)
				for _, tc := range frame.Choices[0].Delta.ToolCalls {
					delta := ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments}
					if tc.Index != nil {
						delta.Index = *tc.Index
					}
					chunk.ToolCalls = append(chunk.ToolCalls, delta)
				}
				if frame.Choices[0].FinishReason != nil {
					chunk.FinishReason = *frame.Choices[0].FinishReason
				}
			}
			if frame.Usage != nil {
				chunk.PromptTokens = frame.Usage.PromptTokens
				chunk.CompletionTokens = frame.Usage.CompletionTokens
				chunk.TotalTokens = frame.Usage.TotalTokens
			}
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			errCh <- errs.Wrap(err, errs.UpstreamUnavailable, "provider stream read failed")
		}
	}()
	return chunks, errCh
}

// Embeddings performs one OpenAI-shaped embeddings call.
func (c *OpenAICompatClient) Embeddings(ctx context.Context, target ProviderTarget, credential, model string, input string) ([]float64, error) {
	payload, err := json.Marshal(map[string]any{"model": model, "input": input})
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "embeddings payload build failed")
	}
	endpoint := strings.TrimSuffix(target.BaseURL, "/") + "/embeddings"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "embeddings request build failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if credential != "" {
		httpReq.Header.Set("Authorization", "Bearer "+credential)
	}
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, errs.Wrap(err, errs.UpstreamUnavailable, "embeddings provider unreachable").AsRetriable()
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, errs.Wrap(err, errs.UpstreamUnavailable, "embeddings read failed")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errs.Wrap(fmt.Errorf("status %d", resp.StatusCode), errs.UpstreamUnavailable, "embeddings provider error")
	}
	var reply struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || len(reply.Data) == 0 {
		return nil, errs.New(errs.UpstreamUnavailable, "embeddings provider returned no vector")
	}
	return reply.Data[0].Embedding, nil
}

// GenerateChatStreamRouted runs the same gates as GenerateChatRouted, then
// streams from the first route whose stream attempt starts successfully.
// ctx cancellation tears down the upstream connection.
func (r *Router) GenerateChatStreamRouted(
	ctx context.Context,
	claims *token.Claims,
	policy PolicyEngine,
	dlp DLPEngine,
	req ChatRequest,
	onAttempt func(RouteAttempt),
) (*ChatResult, <-chan StreamChunk, <-chan error, DecisionTrace, error) {
	trace := DecisionTrace{Outcome: "error"}
	if r.ResolvePlan == nil || r.ResolveTarget == nil || r.Credential == nil {
		return nil, nil, nil, trace, errs.PolicyFailClosed("router is not fully configured")
	}
	streamer, ok := r.Client.(StreamClient)
	if !ok {
		return nil, nil, nil, trace, errs.PolicyFailClosed("router client does not support streaming")
	}
	if _, _, err := authorizeLLM(claims); err != nil {
		return nil, nil, nil, trace, err
	}
	if policy == nil || dlp == nil {
		return nil, nil, nil, trace, errs.PolicyFailClosed("policy or DLP engine is not configured")
	}
	decision, err := policy.Decide(ctx, PolicyInput{
		Session: claimSession{userID: claims.Subject, workspace: claims.WorkspaceID},
		Model:   req.Model, Action: "llm.chat.stream",
	})
	if err != nil {
		return nil, nil, nil, trace, errs.PolicyFailClosed(err.Error())
	}
	trace.Effect, trace.RuleID = decision.Effect, decision.RuleID
	if !decision.Allowed {
		trace.Outcome = "denied"
		return nil, nil, nil, trace, errs.New(errs.PolicyDenied, "policy denied the request: "+decision.Reason)
	}
	effect := decision.Effect
	if effect == "" {
		effect = EffectAllow
	}
	switch effect {
	case EffectDeny, EffectRequireApproval:
		trace.Outcome = "denied"
		return nil, nil, nil, trace, errs.New(errs.PolicyDenied, "policy denied the request: "+decision.Reason)
	case EffectAllow, EffectRedact, EffectLocalModelOnly:
	default:
		return nil, nil, nil, trace, errs.PolicyFailClosed("unknown policy effect: " + effect)
	}
	mode := ScanEnforce
	if effect == EffectRedact {
		mode = ScanRedact
	}
	scan, err := scanMessages(ctx, dlp, req, mode)
	if err != nil {
		return nil, nil, nil, trace, errs.DLPFailClosed(err.Error())
	}
	trace.RequestRedactions = scan.RedactionCount
	trace.DLPRuleNames = scan.RuleNames
	if scan.Blocked {
		trace.Outcome = "dlp_blocked"
		return nil, nil, nil, trace, errs.New(errs.DLPBlocked, "DLP blocked outbound content: "+scan.Reason)
	}
	if scan.RedactionCount > 0 {
		trace.Outcome = "redacted"
	}
	plan, err := r.ResolvePlan(ctx, req.Model)
	if err != nil {
		return nil, nil, nil, trace, err
	}
	var lastErr error
	for _, route := range plan.Routes {
		target, err := r.ResolveTarget(ctx, route.Provider)
		if err != nil {
			lastErr = err
			if onAttempt != nil {
				onAttempt(RouteAttempt{Provider: route.Provider, Model: route.Model, Status: "failed", Err: "target_unavailable"})
			}
			continue
		}
		credential, credErr := r.Credential(ctx, target.CredentialRef)
		if credErr != nil {
			lastErr = errs.Wrap(credErr, errs.Internal, "credential resolution failed")
			if onAttempt != nil {
				onAttempt(RouteAttempt{Provider: route.Provider, Model: route.Model, Status: "failed", Err: "credential_unavailable"})
			}
			continue
		}
		// Same alias->route model rewrite as the non-streaming loop.
		routeReq := req
		routeReq.Model = route.Model
		chunks, errCh := streamer.ChatStream(ctx, target, credential, routeReq)
		if onAttempt != nil {
			onAttempt(RouteAttempt{Provider: route.Provider, Model: route.Model, Status: "selected"})
		}
		if trace.Outcome != "redacted" {
			trace.Outcome = "allowed"
		}
		// Single-reader contract: the caller consumes chunks and aggregates
		// content/usage (a second reader here would race for frames).
		return &ChatResult{Model: req.Model}, chunks, errCh, trace, nil
	}
	if lastErr == nil {
		lastErr = errs.New(errs.UpstreamUnavailable, "all routes exhausted")
	}
	return nil, nil, nil, trace, lastErr
}
