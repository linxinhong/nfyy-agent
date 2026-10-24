package compaction

import (
	"cmp"
	"context"
	"errors"
	"strings"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

const defaultSummarizationPrompt = `You are a conversation summarizer. Produce a concise summary that preserves:
- Key facts, decisions, and user preferences
- Important context needed for future turns
- Tool call outcomes and their significance

Be factual and brief.`

const defaultMinPreservedSummarizationGroups = 8

// Summarizer generates a summary from messages selected by a strategy.
type Summarizer interface {
	Summarize(context.Context, []models.Message) (string, error)
}

// SummarizerFunc adapts a function to Summarizer.
type SummarizerFunc func(context.Context, []models.Message) (string, error)

func (f SummarizerFunc) Summarize(ctx context.Context, msgs []models.Message) (string, error) {
	return f(ctx, msgs)
}

// SummarizationStrategy summarizes older groups into a single assistant
// summary message. System messages and the most recent non-system groups
// are preserved.
type SummarizationStrategy struct {
	Trigger                Trigger
	Target                 Trigger
	Summarizer             Summarizer
	MinimumPreservedGroups int
	SummarizationPrompt    string
	SummaryUnavailableMsg  string
}

// Compact compacts index in place.
func (s *SummarizationStrategy) Compact(ctx context.Context, index *MessageIndex) (bool, error) {
	target, ok := prepareCompaction(index, s.Trigger, s.Target)
	if !ok {
		return false, nil
	}
	if s.Summarizer == nil {
		return false, nil
	}

	minPreserved := cmp.Or(max(s.MinimumPreservedGroups, 0), defaultMinPreservedSummarizationGroups)
	prompt := cmp.Or(s.SummarizationPrompt, defaultSummarizationPrompt)
	unavailable := cmp.Or(s.SummaryUnavailableMsg, "[Summary unavailable]")

	nonSys := 0
	for _, g := range index.Groups {
		if !g.IsExcluded && g.Kind != GroupKindSystem {
			nonSys++
		}
	}
	protected := min(minPreserved, nonSys)
	maxSummarizable := nonSys - protected
	if maxSummarizable <= 0 {
		return false, nil
	}

	messages := []models.Message{{
		Role:    models.RoleSystem,
		Content: prompt,
	}}
	var excludedGroups []*MessageGroup
	insertIndex := -1
	for i, group := range index.Groups {
		if len(excludedGroups) >= maxSummarizable {
			break
		}
		if group.IsExcluded || group.Kind == GroupKindSystem {
			continue
		}
		if insertIndex < 0 {
			insertIndex = i
		}
		messages = append(messages, group.Messages...)
		group.IsExcluded = true
		group.ExcludeReason = "summarized"
		excludedGroups = append(excludedGroups, group)
		if target(index) {
			break
		}
	}

	summaryText, err := s.Summarizer.Summarize(ctx, messages)
	if err != nil {
		for _, g := range excludedGroups {
			g.IsExcluded = false
			g.ExcludeReason = ""
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		return false, nil
	}
	if strings.TrimSpace(summaryText) == "" {
		summaryText = unavailable
	}

	summaryMsg := models.Message{
		Role:                 models.RoleAssistant,
		Content:              "[Summary]\n" + summaryText,
		AdditionalProperties: map[string]any{summaryPropertyKey: true},
	}
	index.InsertGroup(insertIndex, GroupKindSummary, []models.Message{summaryMsg}, nil)
	return true, nil
}
