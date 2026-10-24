package compaction

import (
	"cmp"
	"context"
	"slices"
)

const defaultMinPreservedSlidingWindowTurns = 1

// SlidingWindowStrategy excludes the oldest user turns while preserving
// recent turns. System messages are always preserved.
type SlidingWindowStrategy struct {
	Trigger               Trigger
	Target                Trigger
	MinimumPreservedTurns int
}

// Compact compacts index in place.
func (s *SlidingWindowStrategy) Compact(_ context.Context, index *MessageIndex) (bool, error) {
	target, ok := prepareCompaction(index, s.Trigger, s.Target)
	if !ok {
		return false, nil
	}

	minTurns := cmp.Or(max(s.MinimumPreservedTurns, 0), defaultMinPreservedSlidingWindowTurns)
	turnGroups := map[int][]int{}
	var turnOrder []int
	for i, g := range index.Groups {
		if g.IsExcluded || g.Kind == GroupKindSystem || g.TurnIndex == nil || *g.TurnIndex <= 0 {
			continue
		}
		ti := *g.TurnIndex
		if _, ok := turnGroups[ti]; !ok {
			turnOrder = append(turnOrder, ti)
		}
		turnGroups[ti] = append(turnGroups[ti], i)
	}

	protected := min(minTurns, len(turnOrder))
	protectedTurns := turnOrder[len(turnOrder)-protected:]

	var compacted bool
	for _, ti := range turnOrder {
		if slices.Contains(protectedTurns, ti) {
			continue
		}
		for _, gi := range turnGroups[ti] {
			g := index.Groups[gi]
			g.IsExcluded = true
			g.ExcludeReason = "sliding window"
		}
		compacted = true
		if target(index) {
			break
		}
	}
	return compacted, nil
}
