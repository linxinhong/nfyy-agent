package providers

import "github.com/linxinhong/xingding-agent-runtime/pkg/models"

// CalculateCost returns the estimated USD cost of one model call given its
// token usage, based on the model's per-million-token pricing. Cached input
// tokens are billed at the CacheRead price and tokens written to the cache
// at the CacheWrite price when the model has them.
func CalculateCost(m Model, usage *models.MessageMetrics) float64 {
	if usage == nil {
		return 0
	}
	input := float64(usage.InputTokens)
	cached, written := 0.0, 0.0
	if m.Cost.CacheRead > 0 && usage.CachedInputTokens > 0 && usage.CachedInputTokens <= usage.InputTokens {
		cached = float64(usage.CachedInputTokens)
		input -= cached
	}
	if m.Cost.CacheWrite > 0 && usage.CacheWriteTokens > 0 && float64(usage.CacheWriteTokens) <= input {
		written = float64(usage.CacheWriteTokens)
		input -= written
	}
	return (input*m.Cost.Input + cached*m.Cost.CacheRead + written*m.Cost.CacheWrite +
		float64(usage.OutputTokens)*m.Cost.Output) / 1_000_000
}

// EstimateCost implements models.CostEstimator.
func (c *openAICompletionsChat) EstimateCost(usage *models.MessageMetrics) float64 {
	return CalculateCost(c.model, usage)
}

// EstimateCost implements models.CostEstimator.
func (c *anthropicMessagesChat) EstimateCost(usage *models.MessageMetrics) float64 {
	return CalculateCost(c.model, usage)
}

var (
	_ models.CostEstimator = (*openAICompletionsChat)(nil)
	_ models.CostEstimator = (*anthropicMessagesChat)(nil)
)
