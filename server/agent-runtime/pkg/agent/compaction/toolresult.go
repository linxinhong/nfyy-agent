package compaction

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

const defaultMinPreservedToolResultGroups = 16

// ToolResultStrategy collapses old tool-call groups into assistant summary
// messages. Only tool-call groups outside the protected window are collapsed.
type ToolResultStrategy struct {
	Trigger                Trigger
	Target                 Trigger
	MinimumPreservedGroups int
	ToolCallFormatter      func(*MessageGroup) string
}

// Compact compacts index in place.
func (s *ToolResultStrategy) Compact(_ context.Context, index *MessageIndex) (bool, error) {
	target, ok := prepareCompaction(index, s.Trigger, s.Target)
	if !ok {
		return false, nil
	}

	minPreserved := cmp.Or(max(s.MinimumPreservedGroups, 0), defaultMinPreservedToolResultGroups)
	nonSys := []int{}
	for i, g := range index.Groups {
		if !g.IsExcluded && g.Kind != GroupKindSystem {
			nonSys = append(nonSys, i)
		}
	}
	protectedStart := len(nonSys) - minPreserved
	if protectedStart < 0 {
		protectedStart = 0
	}
	protected := nonSys[protectedStart:]

	eligible := []int{}
	for i, g := range index.Groups {
		if !g.IsExcluded && g.Kind == GroupKindToolCall && !slices.Contains(protected, i) {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		return false, nil
	}

	formatter := s.ToolCallFormatter
	if formatter == nil {
		formatter = DefaultToolCallFormatter
	}

	compacted := false
	offset := 0
	for _, ei := range eligible {
		idx := ei + offset
		g := index.Groups[idx]
		summary := formatter(g)

		g.IsExcluded = true
		g.ExcludeReason = "collapsed"

		summaryMsg := models.Message{
			Role:                 models.RoleAssistant,
			Content:              summary,
			AdditionalProperties: map[string]any{summaryPropertyKey: true},
		}
		index.InsertGroup(idx+1, GroupKindSummary, []models.Message{summaryMsg}, g.TurnIndex)
		offset++
		compacted = true
		if target(index) {
			break
		}
	}
	return compacted, nil
}

// DefaultToolCallFormatter produces a compact YAML-like summary of tool-call
// groups listing tool names and their results.
func DefaultToolCallFormatter(group *MessageGroup) string {
	type call struct{ id, name string }
	var calls []call
	results := map[string]string{}
	var plain []string

	for _, msg := range group.Messages {
		for _, tc := range msg.ToolCalls {
			calls = append(calls, call{id: tc.ID, name: tc.Function.Name})
		}
		if msg.Role == models.RoleTool {
			if msg.Content != "" {
				plain = append(plain, msg.Content)
			}
		}
	}

	pi := 0
	ordered := []string{}
	grouped := map[string][]string{}
	for _, c := range calls {
		if _, ok := grouped[c.name]; !ok {
			ordered = append(ordered, c.name)
		}
		r := results[c.id]
		if r == "" && pi < len(plain) {
			r = plain[pi]
			pi++
		}
		if r != "" {
			grouped[c.name] = append(grouped[c.name], r)
		}
	}

	lines := []string{"[Tool Calls]"}
	for _, name := range ordered {
		lines = append(lines, name+":")
		for _, r := range grouped[name] {
			lines = append(lines, "  - "+r)
		}
	}
	return strings.Join(lines, "\n")
}
