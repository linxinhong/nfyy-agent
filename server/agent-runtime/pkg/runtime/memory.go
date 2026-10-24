package runtime

import (
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	agentos "github.com/linxinhong/xingding-agent-runtime/pkg/os"
)

func buildMemory(spec ProjectSpec, deps Dependencies) (*agentos.MemoryConfig, error) {
	if spec.Memory == nil {
		return nil, nil
	}
	dependency := deps.Memories[spec.Memory.Ref]
	if dependency.Module == nil {
		return nil, fmt.Errorf("memory dependency %q is unavailable", spec.Memory.Ref)
	}
	result := &agentos.MemoryConfig{
		Module: dependency.Module, ProjectID: spec.ID, CollectionID: spec.Memory.CollectionID,
		ProposeEnabled: spec.Memory.ProposeEnabled, SourceResolver: dependency.SourceResolver,
	}
	if spec.Memory.Recall != nil {
		mode := agent.MemoryRecallStrict
		if spec.Memory.Recall.FailureMode == "omit_on_unavailable" {
			mode = agent.MemoryRecallOmitUnavailable
		}
		result.Recall = &agentos.MemoryRecallConfig{
			Limit: spec.Memory.Recall.Limit, Kinds: append([]memory.Kind(nil), spec.Memory.Recall.Kinds...),
			Topics: append([]string(nil), spec.Memory.Recall.Topics...), FailureMode: mode,
		}
	}
	return result, nil
}
