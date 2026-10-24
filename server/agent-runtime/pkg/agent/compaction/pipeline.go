package compaction

import "context"

// PipelineStrategy executes strategies sequentially against the same index.
// Each strategy operates on the result of the previous one, enabling
// composed behaviors such as summarizing older messages and then truncating.
type PipelineStrategy struct {
	Strategies []Strategy
}

// Compact compacts index in place.
func (s *PipelineStrategy) Compact(ctx context.Context, index *MessageIndex) (bool, error) {
	var anyCompacted bool
	for _, child := range s.Strategies {
		if child == nil {
			continue
		}
		compacted, err := child.Compact(ctx, index)
		if err != nil {
			return anyCompacted, err
		}
		if compacted {
			anyCompacted = true
		}
	}
	return anyCompacted, nil
}
