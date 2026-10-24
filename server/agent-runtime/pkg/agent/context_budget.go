package agent

import (
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// defaultContextBudget returns the CompactionContextProvider New appends
// when no compaction is configured, so long sessions are compacted before
// they overflow the model's window instead of failing with a provider 400.
// The budget is the smallest advertised window among the primary and
// fallback models, minus the response reservation: MaxTokens when set,
// otherwise the model's output limit capped at a quarter of the window.
// Tokens are counted with counter, which the agent calibrates against
// provider-reported usage.
func defaultContextBudget(cfg AgentConfig, counter TokenCounter) (*CompactionContextProvider, error) {
	for _, provider := range cfg.ContextProviders {
		if _, ok := provider.(*CompactionContextProvider); ok {
			return nil, nil
		}
	}
	if cfg.ContextBudget != nil {
		if cfg.ContextBudget.MaxInputTokens == 0 {
			return nil, nil
		}
		policy := *cfg.ContextBudget
		if err := validateContextWindowPolicy(policy); err != nil {
			return nil, err
		}
		return &CompactionContextProvider{Policy: &policy, Counter: counter, Summarizer: cfg.ContextSummarizer}, nil
	}
	budget := 0
	for _, m := range append([]models.Model{cfg.Model}, cfg.FallbackModels...) {
		limits, ok := m.(models.ContextLimits)
		if !ok || limits.ContextWindow() <= 0 {
			continue
		}
		window := limits.ContextWindow()
		reserve := limits.MaxOutputTokens()
		if reserve <= 0 || reserve > window/4 {
			reserve = window / 4
		}
		if cfg.MaxTokens != nil && *cfg.MaxTokens > 0 {
			reserve = *cfg.MaxTokens
		}
		if input := window - reserve; input > 0 && (budget == 0 || input < budget) {
			budget = input
		}
	}
	if budget == 0 {
		return nil, nil
	}
	return &CompactionContextProvider{Policy: &ContextWindowPolicy{MaxInputTokens: budget}, Counter: counter, Summarizer: cfg.ContextSummarizer}, nil
}

func validateContextWindowPolicy(p ContextWindowPolicy) error {
	trigger, target := p.TriggerRatio, p.TargetRatio
	if trigger == 0 {
		trigger = 0.8
	}
	if target == 0 {
		target = 0.7
	}
	if p.MaxInputTokens < 0 || p.MinRecentTurns < 0 || trigger < 0 || trigger > 1 || target < 0 || target > 1 || target >= trigger {
		return ErrInvalidContextWindowPolicy
	}
	return nil
}
