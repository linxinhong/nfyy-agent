// Package openai defines the EAP OpenAI-compatible wire contract (v1):
// request/response types for /v1/chat/completions, its SSE chunk shape, and
// /v1/models. Shapes are frozen by golden fixtures under
// contracts/schemas/v1/openai/; changing them is a public contract change.
package openai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ContractVersion identifies the frozen contract generation.
const ContractVersion = "v1"

// ContentPart is one element of a content-parts array. Text parts carry
// Text (governed: flattened into Message.Content for DLP and rewritten in
// place on redaction); every part keeps its original JSON in Raw so the
// array is forwarded structurally to the provider (ADR-0028 decision 2).
type ContentPart struct {
	Type string
	Text string
	Raw  json.RawMessage
}

// Message is one chat message. Inbound content accepts both the plain string
// form and the OpenAI content-parts array form (real agents send arrays);
// text parts are flattened for governance while ContentParts keeps the
// original structure for structural forwarding (ADR-0028 decision 2).
// Tool-calling fields (ADR-0025) pass through unchanged: assistant messages
// may carry tool_calls, tool-role messages carry tool_call_id.
type Message struct {
	Role         string        `json:"role"`
	Content      string        `json:"content"`
	ContentParts []ContentPart `json:"-"`
	Name         string        `json:"name,omitempty"`
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
}

// MarshalJSON emits the assistant tool-call shape with "content": null —
// OpenAI-strict clients reject an empty-string content next to tool_calls.
// When ContentParts is set, content serializes as the original parts array
// (each part byte-faithfully via its Raw JSON).
func (m Message) MarshalJSON() ([]byte, error) {
	wire := struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       string          `json:"name,omitempty"`
		ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
	}{Role: m.Role, Name: m.Name, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	switch {
	case len(m.ContentParts) > 0:
		parts := make([]json.RawMessage, 0, len(m.ContentParts))
		for _, p := range m.ContentParts {
			parts = append(parts, p.Raw)
		}
		b, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		wire.Content = b
	case len(m.ToolCalls) > 0 && m.Content == "":
		// leave content unset → null
	default:
		c := m.Content
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		wire.Content = b
	}
	return json.Marshal(wire)
}

// ToolCall is one function invocation, in assistant messages (complete) and
// in streaming deltas (partial: only index + argument fragment may be set).
type ToolCall struct {
	// Index orders streaming tool_calls deltas; omitted outside streaming.
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// UnmarshalJSON accepts content as a string, an array of content parts
// ([{"type":"text","text":"..."}]), or null, and decodes the tool-calling
// fields alongside it.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       string          `json:"name"`
		ToolCalls  []ToolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Name = raw.Name
	m.ToolCalls = raw.ToolCalls
	m.ToolCallID = raw.ToolCallID
	if len(raw.Content) == 0 || string(raw.Content) == "null" {
		m.Content = ""
		return nil
	}
	var text string
	if err := json.Unmarshal(raw.Content, &text); err == nil {
		m.Content = text
		return nil
	}
	var rawParts []json.RawMessage
	if err := json.Unmarshal(raw.Content, &rawParts); err != nil {
		return fmt.Errorf("message content must be a string or a content-part array")
	}
	var b strings.Builder
	for _, partRaw := range rawParts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(partRaw, &p); err != nil {
			return fmt.Errorf("message content part is not an object: %w", err)
		}
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
		m.ContentParts = append(m.ContentParts, ContentPart{Type: p.Type, Text: p.Text, Raw: partRaw})
	}
	m.Content = b.String()
	return nil
}

// governedRequestFields is the frozen set of /v1/chat/completions request
// fields EAP types and governs (ADR-0028 decision 1). Every OTHER request
// field is captured opaquely in ChatCompletionRequest.Extra and forwarded
// verbatim to the provider; the provider remains the validator of its own
// surface. Adding a field here is a governance decision, not a code cleanup.
var governedRequestFields = map[string]struct{}{
	"model":               {},
	"messages":            {},
	"temperature":         {},
	"top_p":               {},
	"stream":              {},
	"max_tokens":          {},
	"stop":                {},
	"parallel_tool_calls": {},
	"stream_options":      {},
	"tools":               {},
	"tool_choice":         {},
}

// ChatCompletionRequest is the /v1/chat/completions request surface. Tools
// and tool_choice travel as opaque JSON (ADR-0025): EAP governs the
// conversation, not the tool schema. Ungoverned fields (n, penalties,
// response_format, seed, user, metadata, unknown vendor extensions, ...)
// are captured byte-faithfully in Extra and forwarded to the provider
// unchanged (ADR-0028 decision 1); governed fields always win on conflict.
type ChatCompletionRequest struct {
	Model             string          `json:"model"`
	Messages          []Message       `json:"messages"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Stream            *bool           `json:"stream,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Stop              json.RawMessage `json:"stop,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	StreamOptions     json.RawMessage `json:"stream_options,omitempty"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`

	// Extra holds ungoverned request fields, captured opaquely.
	Extra map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the governed fields by alias (avoiding recursion)
// and collects every other key into Extra as raw JSON.
func (r *ChatCompletionRequest) UnmarshalJSON(data []byte) error {
	type governed ChatCompletionRequest
	var g governed
	if err := json.Unmarshal(data, &g); err != nil {
		return err
	}
	*r = ChatCompletionRequest(g)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	for k, v := range keys {
		if _, ok := governedRequestFields[k]; ok {
			continue
		}
		if r.Extra == nil {
			r.Extra = make(map[string]json.RawMessage)
		}
		r.Extra[k] = v
	}
	return nil
}

// MarshalJSON emits the governed fields plus every opaque Extra key
// verbatim. Governed fields win: Extra keys colliding with a governed JSON
// name are dropped.
func (r ChatCompletionRequest) MarshalJSON() ([]byte, error) {
	type governed ChatCompletionRequest
	base, err := json.Marshal(governed(r))
	if err != nil {
		return nil, err
	}
	if len(r.Extra) == 0 {
		return base, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	for k, v := range r.Extra {
		if _, ok := governedRequestFields[k]; ok {
			continue
		}
		m[k] = v
	}
	return json.Marshal(m)
}

// Usage reports token consumption for one call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice is one non-streaming completion alternative.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason,omitempty"`
}

// ChatCompletionResponse is the non-streaming /v1/chat/completions body.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Delta is the per-chunk incremental content. ContentParts carries provider
// deltas that arrived as content-part arrays (ADR-0028 decision 2): when set,
// content serializes as the parts array and must never be text-transformed.
type Delta struct {
	Role         string        `json:"role,omitempty"`
	Content      string        `json:"content,omitempty"`
	ContentParts []ContentPart `json:"-"`
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
}

// MarshalJSON emits content as the original parts array when ContentParts is
// set, otherwise as the plain string form.
func (d Delta) MarshalJSON() ([]byte, error) {
	wire := struct {
		Role      string          `json:"role,omitempty"`
		Content   json.RawMessage `json:"content,omitempty"`
		ToolCalls []ToolCall      `json:"tool_calls,omitempty"`
	}{Role: d.Role, ToolCalls: d.ToolCalls}
	if len(d.ContentParts) > 0 {
		parts := make([]json.RawMessage, 0, len(d.ContentParts))
		for _, p := range d.ContentParts {
			parts = append(parts, p.Raw)
		}
		b, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		wire.Content = b
	} else if d.Content != "" {
		b, err := json.Marshal(d.Content)
		if err != nil {
			return nil, err
		}
		wire.Content = b
	}
	return json.Marshal(wire)
}

// ChunkChoice is one streaming alternative; FinishReason stays null until the
// final chunk, mirroring the upstream protocol.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// ChatCompletionChunk is one SSE data frame of a streaming completion.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
}

// Model is one logical model advertised by GET /v1/models.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelsResponse is the GET /v1/models body.
type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
