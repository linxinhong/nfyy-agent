package compaction

import (
	"context"
	"fmt"
	"math"
)

const (
	defaultToolEvictionThreshold = 0.5
	defaultTruncationThreshold   = 0.8
)

// ContextWindowStrategy derives token thresholds from a model's context
// window size and maximum output tokens, then applies a two-phase pipeline:
//  1. Tool-result eviction (ToolResultStrategy)
//  2. Truncation (TruncationStrategy)
//
// The input budget is MaxContextWindowTokens - MaxOutputTokens.
type ContextWindowStrategy struct {
	MaxContextWindowTokens int
	MaxOutputTokens        int
	ToolEvictionThreshold  float64
	TruncationThreshold    float64
}

// Compact compacts index in place.
func (s *ContextWindowStrategy) Compact(ctx context.Context, index *MessageIndex) (bool, error) {
	tev := s.ToolEvictionThreshold
	if tev == 0 {
		tev = defaultToolEvictionThreshold
	}
	trunc := s.TruncationThreshold
	if trunc == 0 {
		trunc = defaultTruncationThreshold
	}

	if s.MaxContextWindowTokens <= 0 {
		return false, fmt.Errorf("compaction: MaxContextWindowTokens must be positive, got %d", s.MaxContextWindowTokens)
	}
	if s.MaxOutputTokens < 0 || s.MaxOutputTokens >= s.MaxContextWindowTokens {
		return false, fmt.Errorf("compaction: MaxOutputTokens must be in [0, MaxContextWindowTokens), got %d", s.MaxOutputTokens)
	}
	if err := validateThreshold("ToolEvictionThreshold", tev); err != nil {
		return false, err
	}
	if err := validateThreshold("TruncationThreshold", trunc); err != nil {
		return false, err
	}
	if trunc < tev {
		return false, fmt.Errorf("compaction: TruncationThreshold (%g) must be >= ToolEvictionThreshold (%g)", trunc, tev)
	}

	budget := s.MaxContextWindowTokens - s.MaxOutputTokens
	toolTokens := int(float64(budget) * tev)
	truncTokens := int(float64(budget) * trunc)

	pipeline := &PipelineStrategy{
		Strategies: []Strategy{
			&ToolResultStrategy{
				Trigger:                TokensExceed(toolTokens),
				MinimumPreservedGroups: 2,
			},
			&TruncationStrategy{
				Trigger:                TokensExceed(truncTokens),
				MinimumPreservedGroups: 2,
			},
		},
	}
	return pipeline.Compact(ctx, index)
}

func validateThreshold(name string, v float64) error {
	if math.IsNaN(v) || v <= 0 || v > 1 {
		return fmt.Errorf("compaction: %s must be in (0, 1], got %g", name, v)
	}
	return nil
}
