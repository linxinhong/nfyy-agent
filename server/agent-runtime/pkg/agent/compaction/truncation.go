package compaction

import (
	"cmp"
	"context"
)

const defaultMinPreservedTruncationGroups = 32

// TruncationStrategy excludes the oldest non-system groups while preserving
// recent groups. System messages are always preserved.
type TruncationStrategy struct {
	Trigger                Trigger
	Target                 Trigger
	MinimumPreservedGroups int
}

// Compact compacts index in place.
func (s *TruncationStrategy) Compact(_ context.Context, index *MessageIndex) (bool, error) {
	target, ok := prepareCompaction(index, s.Trigger, s.Target)
	if !ok {
		return false, nil
	}

	minPreserved := cmp.Or(max(s.MinimumPreservedGroups, 0), defaultMinPreservedTruncationGroups)
	removable := index.IncludedNonSystemGroupCount()
	maxRemovable := removable - minPreserved
	if maxRemovable <= 0 {
		return false, nil
	}

	var compacted bool
	removed := 0
	for _, g := range index.Groups {
		if removed >= maxRemovable {
			break
		}
		if g.IsExcluded || g.Kind == GroupKindSystem {
			continue
		}
		g.IsExcluded = true
		g.ExcludeReason = "truncated"
		removed++
		compacted = true
		if target(index) {
			break
		}
	}
	return compacted, nil
}
