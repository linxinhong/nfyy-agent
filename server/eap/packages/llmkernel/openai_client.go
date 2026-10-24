package llmkernel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"enterprise-agent-platform/packages/errs"
)

// OpenAICompatClient performs one OpenAI-shaped chat call against a target's
// base URL using the server-resolved credential. The provider-native model
// name travels only in the upstream payload.
type OpenAICompatClient struct {
	Client           *http.Client
	MaxResponseBytes int64
}

func (c *OpenAICompatClient) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// governedPayloadFields mirrors the typed keys of openAIChatPayload: Extra
// keys colliding with any of these are dropped (governed fields win,
// TASK-051/ADR-0028).
var governedPayloadFields = map[string]struct{}{
	"model":               {},
	"messages":            {},
	"stream":              {},
	"temperature":         {},
	"top_p":               {},
	"max_tokens":          {},
	"stop":                {},
	"parallel_tool_calls": {},
	"stream_options":      {},
	"tools":               {},
	"tool_choice":         {},
}

type openAIChatPayload struct {
	Model             string            `json:"model"`
	Messages          []openAIMessageIn `json:"messages"`
	Stream            bool              `json:"stream,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	MaxTokens         *int              `json:"max_tokens,omitempty"`
	Stop              json.RawMessage   `json:"stop,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	StreamOptions     json.RawMessage   `json:"stream_options,omitempty"`
	Tools             json.RawMessage   `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`

	// Extra carries ungoverned request fields, forwarded verbatim
	// (TASK-051/ADR-0028 decision 1).
	Extra map[string]json.RawMessage `json:"-"`
}

// MarshalJSON emits the typed payload plus every Extra key verbatim.
// Governed fields win on key conflict. stream=true with Extra still keeps
// Extra keys in the payload.
func (p openAIChatPayload) MarshalJSON() ([]byte, error) {
	type alias openAIChatPayload
	base, err := json.Marshal(alias(p))
	if err != nil {
		return nil, err
	}
	if len(p.Extra) == 0 {
		return base, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	for k, v := range p.Extra {
		if _, ok := governedPayloadFields[k]; ok {
			continue
		}
		m[k] = v
	}
	return json.Marshal(m)
}

type openAIMessageIn struct {
	Role string `json:"role"`
	// Content carries either the flattened string or, when the inbound
	// message preserved a content-parts structure, the original parts array
	// forwarded structurally (ADR-0028 decision 2).
	Content    any              `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// contentPartsWire returns the provider-bound content value: the original
// parts array (byte-faithful per part) when preserved, otherwise the plain
// string.
func contentPartsWire(parts []ContentPart) any {
	if len(parts) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Raw)
	}
	return out
}

// openAIToolCall mirrors the OpenAI wire shape (nested function object).
type openAIToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

func toolCallsToWire(tcs []ToolCall) []openAIToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]openAIToolCall, 0, len(tcs))
	for _, tc := range tcs {
		wire := openAIToolCall{ID: tc.ID, Type: "function"}
		wire.Function.Name = tc.Name
		wire.Function.Arguments = tc.Arguments
		out = append(out, wire)
	}
	return out
}

type openAIChatReply struct {
	Choices []struct {
		Message struct {
			Content   json.RawMessage  `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat implements ProviderClient.
func (c *OpenAICompatClient) Chat(ctx context.Context, target ProviderTarget, credential string, req ChatRequest) (*ChatResult, error) {
	if target.BaseURL == "" {
		return nil, errs.New(errs.Internal, "provider endpoint is not configured")
	}
	payload := openAIChatPayload{
		Model: req.Model, Tools: req.Tools, ToolChoice: req.ToolChoice,
		Temperature: req.Temperature, TopP: req.TopP, MaxTokens: req.MaxTokens,
		Stop: req.Stop, ParallelToolCalls: req.ParallelToolCalls, StreamOptions: req.StreamOptions,
		Extra: req.Extra,
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
		return nil, errs.Wrap(err, errs.Internal, "provider payload build failed")
	}
	endpoint := strings.TrimSuffix(target.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "provider request build failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if credential != "" {
		httpReq.Header.Set("Authorization", "Bearer "+credential)
	}

	maxBytes := c.MaxResponseBytes
	if maxBytes == 0 {
		maxBytes = 4 << 20
	}
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, errs.Wrap(err, errs.UpstreamUnavailable, "provider unreachable").AsRetriable()
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, errs.Wrap(err, errs.UpstreamUnavailable, "provider read failed")
	}
	if int64(len(raw)) > maxBytes {
		return nil, errs.New(errs.UpstreamUnavailable, "provider response exceeded the size limit")
	}

	var reply openAIChatReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, errs.Wrap(fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(raw), 200)), errs.UpstreamUnavailable, "provider response is not valid JSON")
	}
	if resp.StatusCode != http.StatusOK {
		message := "provider error"
		if reply.Error != nil && reply.Error.Message != "" {
			message = reply.Error.Message
		}
		e := errs.Wrap(fmt.Errorf("status %d: %s", resp.StatusCode, message), errs.UpstreamUnavailable, message)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			e = e.AsRetriable()
		}
		return nil, e
	}
	if len(reply.Choices) == 0 {
		return nil, errs.New(errs.UpstreamUnavailable, "provider returned no choices")
	}
	contentText, contentParts := decodeReplyContent(reply.Choices[0].Message.Content)
	result := &ChatResult{
		Content:      contentText,
		ContentParts: contentParts,
		Model:        req.Model,
		Usage: Usage{
			PromptTokens:     reply.Usage.PromptTokens,
			CompletionTokens: reply.Usage.CompletionTokens,
			TotalTokens:      reply.Usage.TotalTokens,
		},
	}
	if reply.Choices[0].FinishReason != nil {
		result.FinishReason = *reply.Choices[0].FinishReason
	}
	for _, tc := range reply.Choices[0].Message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// decodeReplyContent accepts provider message/delta content as a string, a
// content-parts array, or null, returning the flattened text plus the
// preserved parts (nil when the content was a plain string). Non-text parts
// are preserved for structural forwarding and never text-transformed
// (ADR-0028 decision 2).
func decodeReplyContent(raw json.RawMessage) (string, []ContentPart) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var rawParts []json.RawMessage
	if err := json.Unmarshal(raw, &rawParts); err != nil {
		return string(raw), nil
	}
	var b strings.Builder
	var parts []ContentPart
	for _, partRaw := range rawParts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(partRaw, &p)
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
		parts = append(parts, ContentPart{Type: p.Type, Text: p.Text, Raw: partRaw})
	}
	return b.String(), parts
}

// NewProviderHTTPClient returns the shared HTTP client for provider
// execution (TASK-040). Defaults were accidental: a fresh http.Client per
// construction rode DefaultTransport, whose MaxIdleConnsPerHost=2 closed
// idle connections beyond two concurrent calls to the same provider and
// forced TCP+TLS re-handshakes under load. One shared transport with
// explicit idle-connection limits keeps connections warm; the 30s provider
// timeout matches the previous per-client default.
func NewProviderHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 64
	transport.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}
