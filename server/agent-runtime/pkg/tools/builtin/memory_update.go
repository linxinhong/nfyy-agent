// Package builtin provides built-in tools available to agents, teams, and workflows.
package builtin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// MemoryUpdate creates a user memory using the memory manager from the run
// context.
func MemoryUpdate(ctx context.Context, input string) (string, error) {
	rc, ok := RunContextFromContext(ctx)
	if !ok || rc.Memory == nil {
		return "", errors.New("no memory manager available")
	}
	mem, err := rc.Memory.CreateMemory(ctx, input, rc.UserID, rc.AgentID)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(mem)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// NewMemoryUpdateTool returns a Tool wrapping MemoryUpdate.
func NewMemoryUpdateTool() (*tools.Function, error) {
	return tools.NewFunction("memory_update", "Create a memory about the user.", MemoryUpdate)
}
