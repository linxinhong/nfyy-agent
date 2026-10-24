// Package anthropic defines the EAP Anthropic Messages compatibility contract
// (v1): request/response types for /v1/messages and the SSE event envelope.
// Shapes are frozen by golden fixtures under contracts/schemas/v1/anthropic/.
package anthropic

// ContractVersion identifies the frozen contract generation.
const ContractVersion = "v1"

// Message is one conversation turn. The V1 compatibility subset carries text
// content strings; block arrays are a future contract version.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// MessagesRequest is the /v1/messages request subset. MaxTokens is required
// by the upstream protocol.
type MessagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Stream    *bool     `json:"stream,omitempty"`
}

// TextBlock is one text content block of a response.
type TextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Usage reports token consumption for one call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// MessageResponse is the non-streaming /v1/messages body.
type MessageResponse struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []TextBlock `json:"content"`
	StopReason   string      `json:"stop_reason,omitempty"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        Usage       `json:"usage"`
}

// Delta carries incremental content for streaming events.
type Delta struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// StreamEvent is one SSE event of a streaming messages call. Type selects the
// payload: message_start fills Message, content_block_delta fills Index and
// Delta, and so on.
type StreamEvent struct {
	Type    string           `json:"type"`
	Index   *int             `json:"index,omitempty"`
	Delta   *Delta           `json:"delta,omitempty"`
	Message *MessageResponse `json:"message,omitempty"`
}
