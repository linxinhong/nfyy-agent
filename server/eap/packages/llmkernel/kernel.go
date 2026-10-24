// Package llmkernel embeds Bifrost Core (github.com/maximhq/bifrost/core,
// Apache-2.0) as the EAP LLM provider/protocol execution kernel behind an
// explicit fail-closed security layer (ADR-0004).
//
// Layering rule (non-negotiable): every request crosses
//
//	verified internal token claims (audience + workspace authority)
//	  -> Policy decision (engine failure = deny)
//	  -> DLP scan of outbound content (engine failure = block)
//	  -> Bifrost Core (provider egress)
//
// A failure at any layer is an authorization-shaped denial produced by this
// package — never a generic plugin error, and never a bypass to the provider.
// The employee API key never enters this layer; only audience-scoped internal
// JWT claims do, and the provider credential stays server-side in the kernel
// configuration.
package llmkernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/token"
)

// Provider is the single provider the spike validates egress against.
var Provider = schemas.OpenAI

// Config is the kernel's server-side configuration.
type Config struct {
	// ProviderBaseURL points at the provider (or an internal egress proxy).
	ProviderBaseURL string
	// ProviderKey is the server-side provider credential. Never exposed to
	// employees or Agent clients.
	ProviderKey string
	// Model is the default model the kernel routes to.
	Model string
	// Models whitelists every model the provider key may serve (must include
	// Model). Empty selects only Model.
	Models []string
}

type staticAccount struct {
	provider schemas.ModelProvider
	keys     []schemas.Key
	config   *schemas.ProviderConfig
}

func (a *staticAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{a.provider}, nil
}

func (a *staticAccount) GetKeysForProvider(_ context.Context, providerKey schemas.ModelProvider) ([]schemas.Key, error) {
	if providerKey != a.provider {
		return nil, fmt.Errorf("provider %s is not configured", providerKey)
	}
	return a.keys, nil
}

func (a *staticAccount) GetConfigForProvider(providerKey schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	if providerKey != a.provider {
		return nil, fmt.Errorf("provider %s is not configured", providerKey)
	}
	return a.config, nil
}

// Kernel is the embedded execution kernel.
type Kernel struct {
	bf  *bifrost.Bifrost
	cfg Config
}

// New initializes the embedded kernel against one provider endpoint.
func New(ctx context.Context, cfg Config) (*Kernel, error) {
	if cfg.ProviderBaseURL == "" || cfg.Model == "" {
		return nil, errors.New("llmkernel: provider base url and model are required")
	}
	models := cfg.Models
	if len(models) == 0 {
		models = []string{cfg.Model}
	}
	account := &staticAccount{
		provider: Provider,
		keys: []schemas.Key{{
			ID:     "eap-primary",
			Value:  schemas.SecretVar{Val: cfg.ProviderKey},
			Models: schemas.WhiteList(models),
			Weight: 1,
		}},
		config: &schemas.ProviderConfig{
			NetworkConfig: schemas.NetworkConfig{
				BaseURL:                        cfg.ProviderBaseURL,
				DefaultRequestTimeoutInSeconds: 30,
				MaxRetries:                     0, // retries/fallbacks are requested per-call
			},
		},
	}
	bf, err := bifrost.Init(ctx, schemas.BifrostConfig{Account: account})
	if err != nil {
		return nil, fmt.Errorf("llmkernel: bifrost init: %w", err)
	}
	return &Kernel{bf: bf, cfg: cfg}, nil
}

// ChatRequest is the EAP-side chat request (logical model + messages).
// Tools and ToolChoice are the client's opaque OpenAI JSON (ADR-0025):
// forwarded byte-faithfully on the routed path, rejected loudly on paths
// that cannot honor them.
type ChatRequest struct {
	Model      string
	Messages   []ChatMessage
	FallbackTo string // optional fallback model, same provider
	Tools      json.RawMessage
	ToolChoice json.RawMessage
	// Sampling parameters are passed through verbatim (TASK-042); the
	// provider applies them or rejects what it does not support.
	Temperature       *float64
	TopP              *float64
	MaxTokens         *int
	Stop              json.RawMessage
	ParallelToolCalls *bool
	StreamOptions     json.RawMessage
	// Extra holds ungoverned request fields captured opaquely at the
	// gateway (ADR-0028 decision 1); they are forwarded verbatim to the
	// provider after the governed fields. DLP/policy/audit semantics never
	// depend on these values, and governed fields win on key conflict.
	Extra map[string]json.RawMessage
}

// ContentPart is one element of a content-parts array carried through the
// kernel structurally (ADR-0028 decision 2). Text parts are governed (their
// text is flattened into ChatMessage.Content and rewritten on DLP
// redaction); every part keeps its original JSON in Raw so the provider
// payload preserves the inbound structure.
type ContentPart struct {
	Type string
	Text string
	Raw  json.RawMessage
}

type ChatMessage struct {
	Role    string
	Content string
	// ContentParts preserves the original content-parts structure for
	// structural provider forwarding; Content stays the flattened text that
	// drives DLP. Empty when inbound content was a plain string.
	ContentParts []ContentPart
	// Tool-calling fields (ADR-0025): ToolCalls rides assistant messages,
	// ToolCallID rides tool-role results.
	Name       string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolCall is one function invocation: complete in assistant messages
// (ID/Name/Arguments), partial in streaming deltas (Index only).
type ToolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// ChatResult is the normalized provider response.
type ChatResult struct {
	Content      string
	ContentParts []ContentPart // set when the provider replied with a parts array
	Model        string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// Usage is the normalized token consumption reported by the provider.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// UsageRecorder receives one record per successful provider call. This is the
// usage/cost hook seam; persistence belongs to a later slice.
type UsageRecorder interface {
	Record(ctx context.Context, record UsageRecord)
}

type UsageRecord struct {
	UserID           string
	WorkspaceID      string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Canonical policy effects (ADR-0010). Zero-value Effect is treated as allow.
const (
	EffectAllow           = "allow"
	EffectRedact          = "allow_with_redaction"
	EffectLocalModelOnly  = "local_model_only"
	EffectRequireApproval = "require_approval"
	EffectDeny            = "deny"
)

// PolicyEngine is the shared policy decision point (LLM/MCP/Knowledge use the
// same vocabulary). A returned error MUST be treated as a denial.
type PolicyEngine interface {
	Decide(ctx context.Context, input PolicyInput) (Decision, error)
}

type PolicyInput struct {
	Session identitySession
	Model   string
	Action  string
}

type Decision struct {
	Allowed bool
	Reason  string
	// Effect carries the canonical effect; empty means allow. RuleID anchors
	// the decision to a specific versioned rule for audit.
	Effect string
	RuleID string
}

// ScanMode selects how a scanner treats deny-class matches.
type ScanMode string

const (
	ScanEnforce ScanMode = "enforce" // deny rules block, redact rules redact
	ScanRedact  ScanMode = "redact"  // all matches become redactions
)

// DLPEngine inspects outbound content before provider egress. A returned
// error MUST be treated as a block: uninspected content never leaves.
type DLPEngine interface {
	Scan(ctx context.Context, text string) (DLPResult, error)
}

// ModeScanner is the optional richer contract: rule-driven scanners that can
// downgrade deny-class matches to redactions under allow_with_redaction.
type ModeScanner interface {
	ScanWithMode(ctx context.Context, text string, mode ScanMode) (DLPResult, error)
}

type DLPResult struct {
	Blocked  bool
	Redacted string
	Reason   string
	// RedactionCount reports how many redaction substitutions were applied
	// to Redacted (audit evidence without raw-secret leakage).
	RedactionCount int
	// RuleNames lists the matched rule ids (no raw matched text).
	RuleNames []string
}

// identitySession is the minimal session shape the kernel needs; satisfied by
// identity.Session without importing the wider platform surface here.
type identitySession interface {
	SessionUserID() string
	SessionWorkspaceID() string
}

// Authorize re-checks the verified claims against the LLM data-plane contract:
// exact audience and non-empty workspace authority. This is authorization
// logic in EAP code — not a plugin warning.
func authorizeLLM(claims *token.Claims) (userID, workspace string, err error) {
	if claims == nil {
		return "", "", errs.New(errs.Unauthenticated, "no verified token claims")
	}
	if claims.Audience != token.AudienceLLMGateway {
		return "", "", errs.New(errs.PermissionDenied,
			"token audience is not valid for the LLM data plane")
	}
	if strings.TrimSpace(claims.WorkspaceID) == "" {
		return "", "", errs.WorkspaceFailClosed("token carries no workspace authority")
	}
	return claims.Subject, claims.WorkspaceID, nil
}

// GenerateChat runs the fail-closed pipeline and, only if every gate passes,
// forwards the request to the provider through Bifrost Core.
func (k *Kernel) GenerateChat(
	ctx context.Context,
	claims *token.Claims,
	policy PolicyEngine,
	dlp DLPEngine,
	usage UsageRecorder,
	req ChatRequest,
) (*ChatResult, error) {
	if err := rejectTools(req); err != nil {
		return nil, err
	}
	userID, workspace, err := authorizeLLM(claims)
	if err != nil {
		return nil, err
	}
	if policy == nil || dlp == nil {
		return nil, errs.PolicyFailClosed("policy or DLP engine is not configured")
	}

	decision, err := policy.Decide(ctx, PolicyInput{
		Session: claimSession{userID: userID, workspace: workspace},
		Model:   k.modelOrDefault(req),
		Action:  "llm.chat",
	})
	if err != nil {
		return nil, errs.PolicyFailClosed(err.Error())
	}
	if !decision.Allowed {
		return nil, errs.New(errs.PolicyDenied, "policy denied the request: "+decision.Reason)
	}

	scan, err := dlp.Scan(ctx, renderContent(req))
	if err != nil {
		return nil, errs.DLPFailClosed(err.Error())
	}
	if scan.Blocked {
		return nil, errs.New(errs.DLPBlocked, "DLP blocked outbound content: "+scan.Reason)
	}

	bReq := k.buildBifrostRequest(req, scan)
	bCtx := schemas.NewBifrostContext(contextWithEAPValues(ctx, userID, workspace), time.Time{})

	resp, bErr := k.bf.ChatCompletionRequest(bCtx, bReq)
	if bErr != nil {
		return nil, normalizeBifrostError(bErr)
	}

	result := &ChatResult{Model: resp.Model}
	if len(resp.Choices) > 0 && resp.Choices[0].Message.Content != nil &&
		resp.Choices[0].Message.Content.ContentStr != nil {
		result.Content = *resp.Choices[0].Message.Content.ContentStr
	}
	if resp.Usage != nil {
		result.Usage = Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
		if usage != nil {
			usage.Record(ctx, UsageRecord{
				UserID:           userID,
				WorkspaceID:      workspace,
				Model:            resp.Model,
				PromptTokens:     result.Usage.PromptTokens,
				CompletionTokens: result.Usage.CompletionTokens,
				TotalTokens:      result.Usage.TotalTokens,
			})
		}
	}
	return result, nil
}

// StreamChat streams provider chunks through the same fail-closed gates and
// returns the raw chunk channel; cancelling ctx stops the stream.
func (k *Kernel) StreamChat(
	ctx context.Context,
	claims *token.Claims,
	policy PolicyEngine,
	dlp DLPEngine,
	req ChatRequest,
) (<-chan *schemas.BifrostStreamChunk, error) {
	if err := rejectTools(req); err != nil {
		return nil, err
	}
	if _, _, err := authorizeLLM(claims); err != nil {
		return nil, err
	}
	if policy == nil || dlp == nil {
		return nil, errs.PolicyFailClosed("policy or DLP engine is not configured")
	}
	decision, err := policy.Decide(ctx, PolicyInput{Action: "llm.chat.stream"})
	if err != nil {
		return nil, errs.PolicyFailClosed(err.Error())
	}
	if !decision.Allowed {
		return nil, errs.New(errs.PolicyDenied, "policy denied the request: "+decision.Reason)
	}
	scan, err := dlp.Scan(ctx, renderContent(req))
	if err != nil {
		return nil, errs.DLPFailClosed(err.Error())
	}
	if scan.Blocked {
		return nil, errs.New(errs.DLPBlocked, "DLP blocked outbound content: "+scan.Reason)
	}

	bCtx := schemas.NewBifrostContext(ctx, time.Time{})
	chunks, bErr := k.bf.ChatCompletionStreamRequest(bCtx, k.buildBifrostRequest(req, scan))
	if bErr != nil {
		return nil, normalizeBifrostError(bErr)
	}
	return chunks, nil
}

func (k *Kernel) modelOrDefault(req ChatRequest) string {
	if req.Model != "" {
		return req.Model
	}
	return k.cfg.Model
}

func (k *Kernel) buildBifrostRequest(req ChatRequest, scan DLPResult) *schemas.BifrostChatRequest {
	model := k.modelOrDefault(req)
	messages := make([]schemas.ChatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		content := m.Content
		messages = append(messages, schemas.ChatMessage{
			Role:    schemas.ChatMessageRole(m.Role),
			Content: &schemas.ChatMessageContent{ContentStr: &content},
		})
	}
	out := &schemas.BifrostChatRequest{
		Provider: Provider,
		Model:    model,
		Input:    messages,
	}
	if req.FallbackTo != "" && req.FallbackTo != model {
		out.Fallbacks = []schemas.Fallback{{Provider: Provider, Model: req.FallbackTo}}
	}
	return out
}

// rejectTools fails loudly (ADR-0025) on paths that cannot honor tool
// calling: silently dropping tools turns tool-using agents into hallucinating
// ones, which is worse than a clean error.
func rejectTools(req ChatRequest) error {
	if len(req.Tools) > 0 || len(req.ToolChoice) > 0 {
		return errs.New(errs.InvalidArgument, "tool calling requires the routed provider path")
	}
	for _, m := range req.Messages {
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			return errs.New(errs.InvalidArgument, "tool calling requires the routed provider path")
		}
	}
	return nil
}

// renderContent concatenates outbound message content for DLP inspection,
// including tool-call arguments (provider-bound conversation data).
func renderContent(req ChatRequest) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(m.Content)
		for _, tc := range m.ToolCalls {
			sb.WriteString(tc.Arguments)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// normalizeBifrostError maps kernel/provider errors onto the frozen EAP error
// vocabulary. Provider auth failures are OUR infrastructure problem (the
// client cannot fix them), so they surface as upstream unavailability.
func normalizeBifrostError(bErr *schemas.BifrostError) error {
	if bErr == nil {
		return errs.New(errs.Internal, "unknown kernel error")
	}
	message := "provider call failed"
	if bErr.Error != nil && bErr.Error.Message != "" {
		message = bErr.Error.Message
	}
	status := 0
	if bErr.StatusCode != nil {
		status = *bErr.StatusCode
	}
	code := errs.UpstreamUnavailable
	switch {
	case status == 400 || status == 404 || status == 422:
		code = errs.InvalidArgument
	case status == 429:
		code = errs.RateLimited
	case status == 401 || status == 403:
		code = errs.UpstreamUnavailable
	case status >= 500 || status == 0:
		code = errs.UpstreamUnavailable
	}
	e := errs.Wrap(errors.New(message), code, message)
	if code == errs.RateLimited || code == errs.UpstreamUnavailable {
		e = e.AsRetriable()
	}
	return e
}

// claimSession adapts verified claims to the minimal session interface.
type claimSession struct {
	userID    string
	workspace string
}

func (c claimSession) SessionUserID() string      { return c.userID }
func (c claimSession) SessionWorkspaceID() string { return c.workspace }

type eapContextKey struct{}

func contextWithEAPValues(ctx context.Context, userID, workspace string) context.Context {
	return context.WithValue(ctx, eapContextKey{}, map[string]string{
		"user_id": userID, "workspace": workspace,
	})
}
