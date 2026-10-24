// Package providers implements pi-ai style model provider management:
// API dialects (wire protocols) are shared implementations, and providers
// are thin registrations that map model catalogs onto a dialect.
//
// The agent-facing seam stays pkg/models.Model; this package only builds
// values that implement it, so agents, teams, and workflows are unaffected.
package providers

import (
	"fmt"
	"net/http"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// API identifies a model API dialect (wire protocol).
type API string

const (
	// APIOpenAICompletions is the OpenAI chat completions dialect, also used
	// by OpenAI-compatible endpoints (DeepSeek, Moonshot, OpenRouter, ...).
	APIOpenAICompletions API = "openai-completions"
	// APIAnthropicMessages is the Anthropic messages dialect.
	APIAnthropicMessages API = "anthropic-messages"
)

// Cost is per-million-token pricing in USD.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// Model is a self-contained value object describing one servable model.
// It carries everything a dialect needs to make a request, so no registry
// lookups happen mid-request.
//
// BaseURL is an arbitrary caller-supplied endpoint: it is NOT governed by
// the managed endpoint registry and must only be used under the
// ProfileLegacyEmbedded profile. Managed deployments resolve models via
// pkg/models/registry (HTTPS-only, no userinfo, approved route set).
type Model struct {
	ID       string
	Name     string
	API      API
	Provider string
	// BaseURL is an ungoverned endpoint (legacy only).
	BaseURL         string
	Reasoning       bool
	Cost            Cost
	ContextWindow   int
	MaxOutputTokens int
	Headers         map[string]string
	// Compat overrides dialect compatibility detection. Only meaningful for
	// APIOpenAICompletions models.
	Compat *OpenAICompletionsCompat
	// DisablePromptCache stops the dialect from marking cache breakpoints
	// (Anthropic cache_control). Providers that cache automatically
	// (OpenAI, DeepSeek) are unaffected.
	DisablePromptCache bool
}

// Ref returns the canonical "provider:id" reference.
func (m Model) Ref() string {
	return m.Provider + ":" + m.ID
}

// ChatOptions configures the runtime chat client derived from a Model.
type ChatOptions struct {
	// APIKey is the credential sent to the provider. May be empty for
	// endpoints that do not require authentication.
	APIKey string
	// Timeout bounds each HTTP call when HTTPClient is nil. Defaults to 120s.
	Timeout time.Duration
	// HTTPClient replaces the internal HTTP client entirely.
	HTTPClient *http.Client
}

// Chat builds a runtime models.Model for this model by dispatching on the
// model's API dialect.
func (m Model) Chat(opts ChatOptions) (models.Model, error) {
	switch m.API {
	case APIOpenAICompletions:
		return newOpenAICompletionsChat(m, opts), nil
	case APIAnthropicMessages:
		return newAnthropicMessagesChat(m, opts), nil
	default:
		return nil, fmt.Errorf("providers: unsupported API dialect %q", m.API)
	}
}

// ContextWindow implements models.ContextLimits.
func (c *openAICompletionsChat) ContextWindow() int { return c.model.ContextWindow }

// MaxOutputTokens implements models.ContextLimits.
func (c *openAICompletionsChat) MaxOutputTokens() int { return c.model.MaxOutputTokens }

// ContextWindow implements models.ContextLimits.
func (c *anthropicMessagesChat) ContextWindow() int { return c.model.ContextWindow }

// MaxOutputTokens implements models.ContextLimits.
func (c *anthropicMessagesChat) MaxOutputTokens() int { return c.model.MaxOutputTokens }

var (
	_ models.ContextLimits = (*openAICompletionsChat)(nil)
	_ models.ContextLimits = (*anthropicMessagesChat)(nil)
)
