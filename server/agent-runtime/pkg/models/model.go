// Package models defines the provider-agnostic model interface and message types.
package models

import "context"

// Role is a chat message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall represents a tool invocation requested by the model.
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function,omitempty"`
	Index    int          `json:"index,omitempty"`
}

// FunctionCall is the payload of a tool call.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolFunction is the function definition of a tool available to the model.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// Tool is a tool definition passed to the model in ModelOptions.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// Image represents an image input or output.
type Image struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// Audio represents an audio input or output.
type Audio struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// Video represents a video input or output.
type Video struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// File represents a file input or output.
type File struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Name     string `json:"name,omitempty"`
}

// Message is a single turn in the chat history.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`
	// Contents is the polymorphic content representation. When set,
	// Content is derived from the text parts. When only Content is set,
	// Normalize() synthesizes a single TextContent. The two fields coexist;
	// consumers should prefer Normalize() for uniform access.
	Contents             []Content      `json:"contents,omitempty"`
	AdditionalProperties map[string]any `json:"additional_properties,omitempty"`
	ToolCalls            []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID           string         `json:"tool_call_id,omitempty"`
	Name                 string         `json:"name,omitempty"`
	Images               []Image        `json:"-"`
	Audios               []Audio        `json:"-"`
	Videos               []Video        `json:"-"`
	Files                []File         `json:"-"`
	ReasoningContent     string         `json:"reasoning_content,omitempty"`
}

// MessageMetrics captures token usage for a single model response.
type MessageMetrics struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
	// CachedInputTokens is the subset of InputTokens served from the
	// provider prompt cache (OpenAI cached_tokens, DeepSeek
	// prompt_cache_hit_tokens).
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	// CacheWriteTokens is the subset of InputTokens written to the prompt
	// cache (Anthropic cache_creation_input_tokens), billed at a premium.
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	// ReasoningTokens is the subset of OutputTokens spent on reasoning.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// ModelResponse is the provider-agnostic output of a model call.
type ModelResponse struct {
	Content          string          `json:"content,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	Usage            *MessageMetrics `json:"usage,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Parsed           any             `json:"parsed,omitempty"`
	ProviderData     any             `json:"provider_data,omitempty"`
	// FinishReason is why generation stopped; Unknown when not reported.
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}

// StreamEvent is one event emitted by ResponseStream. Event is one of
// "content_delta", "reasoning_delta", "tool_call_delta", "usage", "finish",
// "error", or "done". A "finish" event carries FinishReason.
type StreamEvent struct {
	Event            string
	Content          string
	ReasoningContent string
	ToolCall         *ToolCall
	Usage            *MessageMetrics
	FinishReason     FinishReason
	Err              error
	Done             bool
}

// ResponseFormatType selects how structured outputs are requested.
type ResponseFormatType string

const (
	ResponseFormatDefault    ResponseFormatType = ""
	ResponseFormatJSON       ResponseFormatType = "json_object"
	ResponseFormatJSONSchema ResponseFormatType = "json_schema"
)

// ResponseFormat describes the desired structured output format.
type ResponseFormat struct {
	Type       ResponseFormatType `json:"type"`
	JSONSchema map[string]any     `json:"json_schema,omitempty"`
}

// ToolChoiceMode selects how the model is allowed to use tools.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
)

// ToolChoice controls whether and which tool the model must call.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"`
}

// ModelOptions carries per-call options forwarded to the provider.
type ModelOptions struct {
	Temperature    *float32
	MaxTokens      *int
	TopP           *float32
	Stop           []string
	ResponseFormat *ResponseFormat
	Tools          []Tool
	ToolChoice     *ToolChoice
}

// Model is the abstract language model.
type Model interface {
	// Response returns a complete model response.
	Response(ctx context.Context, messages []Message, opts ModelOptions) (*ModelResponse, error)
	// ResponseStream returns a channel of incremental stream events.
	ResponseStream(ctx context.Context, messages []Message, opts ModelOptions) (<-chan StreamEvent, error)
	// ID returns the model identifier, e.g. "gpt-4o".
	ID() string
	// Provider returns the provider name, e.g. "openai".
	Provider() string
}

// ContextLimits is an optional capability for models that advertise their
// context window and maximum output tokens (zero means unknown). The agent
// uses it to derive a default context budget.
type ContextLimits interface {
	ContextWindow() int
	MaxOutputTokens() int
}

// EphemeralContextProperty marks, in Message.AdditionalProperties, a
// system message whose content changes per request (session state,
// memories, retrieval results, current time). Dialects that cache prompt
// prefixes keep such messages out of the cached system prompt.
const EphemeralContextProperty = "xingding.ephemeral_context"

// IsEphemeralContext reports whether m carries EphemeralContextProperty.
func IsEphemeralContext(m Message) bool {
	v, _ := m.AdditionalProperties[EphemeralContextProperty].(bool)
	return v
}

// CostEstimator is an optional capability for models that know their
// pricing; EstimateCost returns the USD cost of one call's usage.
type CostEstimator interface {
	EstimateCost(usage *MessageMetrics) float64
}

// JSONSchemaCapable is an optional capability that models may implement to
// advertise native json_schema response format support. When a model does not
// implement this interface or returns false, the agent falls back to
// json_object for structured output requests.
type JSONSchemaCapable interface {
	SupportsNativeJSONSchema() bool
}
