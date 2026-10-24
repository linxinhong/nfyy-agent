package compaction

import (
	"context"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// Strategy compacts a message index to reduce context size. Strategies mutate
// the index in place by marking groups as excluded or inserting compact
// replacement groups such as summaries.
type Strategy interface {
	Compact(context.Context, *MessageIndex) (bool, error)
}

// prepareCompaction is a helper used by most strategies to validate the index
// and resolve trigger/target defaults.
func prepareCompaction(index *MessageIndex, trigger, target Trigger) (Trigger, bool) {
	if index == nil {
		panic("compaction: index is required")
	}
	if trigger == nil {
		trigger = Always()
	}
	if target == nil {
		target = func(idx *MessageIndex) bool { return !trigger(idx) }
	}
	if index.IncludedNonSystemGroupCount() <= 1 || !trigger(index) {
		return nil, false
	}
	return target, true
}

// Compact applies a strategy to messages and returns the included compacted
// messages. Useful for ad-hoc compaction outside of a context provider.
func Compact(ctx context.Context, strategy Strategy, messages []models.Message, counter TokenCounter) ([]models.Message, error) {
	if strategy == nil {
		panic("compaction: strategy is required")
	}
	idx := CreateMessageIndex(messages, counter)
	if _, err := strategy.Compact(ctx, idx); err != nil {
		return nil, err
	}
	return idx.IncludedMessages(), nil
}
