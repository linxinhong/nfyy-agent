// Package openai implements the OpenAI model provider using only net/http.
//
// Deprecated: this package is a thin compatibility wrapper over
// pkg/providers. New code should use providers directly, e.g.
// providers.Resolve("openai:gpt-4o-mini").
package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/providers"
)

// OpenAIChat implements models.Model using the OpenAI chat completions API.
//
// Deprecated: use providers.Resolve or providers.NewFromEnv().Resolve.
type OpenAIChat struct {
	modelID    string
	apiKey     string
	baseURL    string
	timeout    time.Duration
	httpClient *http.Client
}

// NewChat creates a new OpenAI chat model.
//
// Deprecated: use providers.Resolve("openai:<model>").
func NewChat(modelID string, apiKey string) *OpenAIChat {
	return &OpenAIChat{
		modelID: modelID,
		apiKey:  apiKey,
		timeout: 120 * time.Second,
	}
}

// SetBaseURL configures a custom base URL for the OpenAI API.
func (c *OpenAIChat) SetBaseURL(baseURL string) *OpenAIChat {
	c.baseURL = baseURL
	return c
}

// SetTimeout configures the HTTP timeout used when no custom client is set.
func (c *OpenAIChat) SetTimeout(timeout time.Duration) *OpenAIChat {
	c.timeout = timeout
	return c
}

// SetHTTPClient replaces the internal HTTP client.
func (c *OpenAIChat) SetHTTPClient(client *http.Client) *OpenAIChat {
	c.httpClient = client
	return c
}

// SupportsNativeJSONSchema reports whether the configured endpoint supports
// strict json_schema response formats.
func (c *OpenAIChat) SupportsNativeJSONSchema() bool {
	m, err := c.build()
	if err != nil {
		return false
	}
	if capable, ok := m.(models.JSONSchemaCapable); ok {
		return capable.SupportsNativeJSONSchema()
	}
	return false
}

// ID returns the model identifier.
func (c *OpenAIChat) ID() string {
	return c.modelID
}

// Provider returns the provider name.
func (c *OpenAIChat) Provider() string {
	return "openai"
}

// Response returns a complete model response.
func (c *OpenAIChat) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	m, err := c.build()
	if err != nil {
		return nil, err
	}
	return m.Response(ctx, messages, opts)
}

// ResponseStream returns a channel of incremental stream events.
func (c *OpenAIChat) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	m, err := c.build()
	if err != nil {
		return nil, err
	}
	return m.ResponseStream(ctx, messages, opts)
}

func (c *OpenAIChat) build() (models.Model, error) {
	m := providers.Model{
		ID:       c.modelID,
		API:      providers.APIOpenAICompletions,
		Provider: "openai",
		BaseURL:  c.baseURL,
	}
	return m.Chat(providers.ChatOptions{
		APIKey:     c.apiKey,
		Timeout:    c.timeout,
		HTTPClient: c.httpClient,
	})
}

// decodeRequest is a test helper kept close to the wrapper for parity with
// the old package-level tests.
func decodeRequest(body []byte) (map[string]any, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return req, nil
}
