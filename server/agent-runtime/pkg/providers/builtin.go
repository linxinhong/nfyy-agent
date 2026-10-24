package providers

// BuiltinProviders returns the provider registrations shipped with this
// package. Each is a thin entry mapping a model catalog onto a shared API
// dialect. Cost figures are approximate list prices in USD per million
// tokens and may drift; verify against the provider's pricing page before
// relying on CalculateCost. Unlisted model IDs under these providers still
// resolve via model synthesis, so catalogs only cover well-known models.
func BuiltinProviders() []Provider {
	return []Provider{
		OpenAI(),
		DeepSeek(),
		OpenRouter(),
		Moonshot(),
		SiliconFlow(),
		Volcengine(),
		Qwen(),
		Anthropic(),
	}
}

// NewFromEnv returns a registry holding the builtin providers, with API
// keys resolved from their environment variables at Resolve time.
func NewFromEnv() *Registry {
	return NewRegistry(BuiltinProviders()...)
}

// OpenAI registers the official OpenAI chat completions endpoint.
func OpenAI() Provider {
	return Provider{
		ID:        "openai",
		Name:      "OpenAI",
		BaseURL:   openAIBaseURL,
		APIKeyEnv: []string{"OPENAI_API_KEY"},
		Models: []Model{
			{ID: "gpt-4o", API: APIOpenAICompletions, Reasoning: false,
				Cost: Cost{Input: 2.50, Output: 10.00}, ContextWindow: 128_000, MaxOutputTokens: 16_384},
			{ID: "gpt-4o-mini", API: APIOpenAICompletions,
				Cost: Cost{Input: 0.15, Output: 0.60}, ContextWindow: 128_000, MaxOutputTokens: 16_384},
		},
	}
}

// DeepSeek registers the official DeepSeek OpenAI-compatible endpoint.
//
//   - deepseek-chat is the DeepSeek-V3 series in non-thinking mode (the API
//     alias tracks the latest V3.x release).
//   - deepseek-reasoner is the R1 line / V3.x thinking mode; it returns
//     reasoning_content, which the dialect parses and replays on tool-call
//     turns as the API requires. Sampling parameters are accepted but
//     ignored by the API.
//
// Thinking can also be switched on for deepseek-chat per call with
// models.WithReasoningEffort. Other hosts serving DeepSeek weights
// (SiliconFlow, Volcengine Ark, DashScope, self-hosted vLLM/Ollama) are
// detected by model ID and get the same reasoning handling, including
// <think> tag parsing for R1.
func DeepSeek() Provider {
	deepseekCost := Cost{Input: 0.28, Output: 0.42, CacheRead: 0.028}
	return Provider{
		ID:        "deepseek",
		Name:      "DeepSeek",
		BaseURL:   "https://api.deepseek.com/v1",
		APIKeyEnv: []string{"DEEPSEEK_API_KEY"},
		Models: []Model{
			{ID: "deepseek-chat", Name: "DeepSeek-V3 (non-thinking)", API: APIOpenAICompletions,
				Cost: deepseekCost, ContextWindow: 128_000, MaxOutputTokens: 8_192},
			{ID: "deepseek-reasoner", Name: "DeepSeek-R1 / V3 thinking", API: APIOpenAICompletions, Reasoning: true,
				Cost: deepseekCost, ContextWindow: 128_000, MaxOutputTokens: 65_536},
		},
	}
}

// Volcengine registers Volcengine Ark (火山方舟), a common host for DeepSeek
// V3/R1 in mainland China. Model IDs are Ark model or endpoint IDs, e.g.
// "volcengine:deepseek-r1-250528" or "volcengine:deepseek-v3-250324".
func Volcengine() Provider {
	return Provider{
		ID:        "volcengine",
		Name:      "Volcengine Ark",
		BaseURL:   "https://ark.cn-beijing.volces.com/api/v3",
		APIKeyEnv: []string{"ARK_API_KEY", "VOLCENGINE_API_KEY"},
	}
}

// OpenRouter registers the OpenRouter gateway. Its catalog is far too large
// to ship, so no models are listed: resolve any model ID (e.g.
// "openrouter:openai/gpt-4o") and a Model is synthesized from provider
// defaults. Attribution headers can be set per model via Model.Headers.
func OpenRouter() Provider {
	return Provider{
		ID:        "openrouter",
		Name:      "OpenRouter",
		BaseURL:   "https://openrouter.ai/api/v1",
		APIKeyEnv: []string{"OPENROUTER_API_KEY"},
	}
}

// Moonshot registers the Moonshot AI (Kimi) OpenAI-compatible endpoint.
func Moonshot() Provider {
	return Provider{
		ID:        "moonshot",
		Name:      "Moonshot AI",
		BaseURL:   "https://api.moonshot.cn/v1",
		APIKeyEnv: []string{"MOONSHOT_API_KEY"},
		Models: []Model{
			{ID: "moonshot-v1-8k", API: APIOpenAICompletions, ContextWindow: 8_192, MaxOutputTokens: 8_192},
			{ID: "moonshot-v1-32k", API: APIOpenAICompletions, ContextWindow: 32_768, MaxOutputTokens: 8_192},
			{ID: "moonshot-v1-128k", API: APIOpenAICompletions, ContextWindow: 131_072, MaxOutputTokens: 8_192},
		},
	}
}

// SiliconFlow registers the SiliconFlow OpenAI-compatible endpoint. Model
// IDs are namespaced (e.g. "siliconflow:deepseek-ai/DeepSeek-V3").
func SiliconFlow() Provider {
	return Provider{
		ID:        "siliconflow",
		Name:      "SiliconFlow",
		BaseURL:   "https://api.siliconflow.cn/v1",
		APIKeyEnv: []string{"SILICONFLOW_API_KEY"},
	}
}

// Qwen registers the Alibaba Qwen (Tongyi Qianwen) OpenAI-compatible endpoint.
// DashScope also serves DeepSeek weights, e.g. "qwen:deepseek-r1".
func Qwen() Provider {
	return Provider{
		ID:        "qwen",
		Name:      "Qwen (Tongyi Qianwen)",
		BaseURL:   "https://dashscope.aliyuncs.com/compatible-mode/v1",
		APIKeyEnv: []string{"DASHSCOPE_API_KEY"},
		Models: []Model{
			{ID: "qwen-max", API: APIOpenAICompletions,
				Cost: Cost{Input: 2.40, Output: 9.60}, ContextWindow: 32_768, MaxOutputTokens: 8_192},
			{ID: "qwen-plus", API: APIOpenAICompletions,
				Cost: Cost{Input: 0.80, Output: 2.00}, ContextWindow: 131_072, MaxOutputTokens: 8_192},
			{ID: "qwen-turbo", API: APIOpenAICompletions,
				Cost: Cost{Input: 0.30, Output: 0.60}, ContextWindow: 1_000_000, MaxOutputTokens: 8_192},
		},
	}
}

// Anthropic registers the Anthropic messages API.
func Anthropic() Provider {
	return Provider{
		ID:        "anthropic",
		Name:      "Anthropic",
		BaseURL:   anthropicBaseURL,
		APIKeyEnv: []string{"ANTHROPIC_API_KEY"},
		Models: []Model{
			{ID: "claude-sonnet-4-20250514", Name: "Claude Sonnet 4", API: APIAnthropicMessages, Reasoning: true,
				Cost: Cost{Input: 3.00, Output: 15.00, CacheRead: 0.30, CacheWrite: 3.75}, ContextWindow: 200_000, MaxOutputTokens: 64_000},
			{ID: "claude-opus-4-1-20250805", Name: "Claude Opus 4.1", API: APIAnthropicMessages, Reasoning: true,
				Cost: Cost{Input: 15.00, Output: 75.00, CacheRead: 1.50, CacheWrite: 18.75}, ContextWindow: 200_000, MaxOutputTokens: 32_000},
			{ID: "claude-3-5-haiku-20241022", Name: "Claude 3.5 Haiku", API: APIAnthropicMessages,
				Cost: Cost{Input: 0.80, Output: 4.00, CacheRead: 0.08, CacheWrite: 1.00}, ContextWindow: 200_000, MaxOutputTokens: 8_192},
		},
	}
}
