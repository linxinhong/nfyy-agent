// Package openai implements an OpenAI-compatible model provider.
package openai

// OpenAILike is an OpenAI-compatible provider that uses a custom BaseURL.
type OpenAILike struct {
	// OpenAIChat provides the underlying OpenAI chat client.
	*OpenAIChat
	// DefaultModel is the model identifier used when none is supplied.
	DefaultModel string
}

// NewLike creates an OpenAI-compatible model with a custom base URL.
func NewLike(apiKey string, baseURL string, defaultModel string) *OpenAILike {
	chat := NewChat(defaultModel, apiKey)
	chat.SetBaseURL(baseURL)
	return &OpenAILike{
		OpenAIChat:   chat,
		DefaultModel: defaultModel,
	}
}

// ID returns the model identifier, falling back to DefaultModel if unset.
func (l *OpenAILike) ID() string {
	if id := l.OpenAIChat.ID(); id != "" {
		return id
	}
	return l.DefaultModel
}

// Provider returns the provider name.
func (l *OpenAILike) Provider() string {
	return "openai-like"
}
