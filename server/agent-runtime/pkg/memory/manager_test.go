package memory_test

import (
	"context"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

func TestManager_CreateAndGetMemories(t *testing.T) {
	ctx := context.Background()
	store := in_memory.New()
	mgr := memory.NewManager(store)

	mem, err := mgr.CreateMemory(ctx, "I enjoy writing Go code", "user-1", "agent-1")
	if err != nil {
		t.Fatalf("CreateMemory failed: %v", err)
	}
	if mem == nil {
		t.Fatal("CreateMemory returned nil memory")
	}
	if mem.Memory != "I enjoy writing Go code" {
		t.Fatalf("unexpected memory content: %s", mem.Memory)
	}
	if mem.Input != mem.Memory {
		t.Fatalf("expected input to be stored as raw input")
	}
	if mem.UserID != "user-1" || mem.AgentID != "agent-1" {
		t.Fatalf("unexpected user/agent: %s/%s", mem.UserID, mem.AgentID)
	}
	if mem.MemoryID == "" {
		t.Fatal("memory id was not generated")
	}

	memories, err := mgr.GetMemories(ctx, "user-1", "agent-1", 10)
	if err != nil {
		t.Fatalf("GetMemories failed: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(memories))
	}
	if memories[0].MemoryID != mem.MemoryID {
		t.Fatalf("memory ids do not match")
	}
}

func TestManager_GetMemoriesLimit(t *testing.T) {
	ctx := context.Background()
	store := in_memory.New()
	mgr := memory.NewManager(store)

	for i := 0; i < 5; i++ {
		if _, err := mgr.CreateMemory(ctx, "memory", "user-limit", "agent-limit"); err != nil {
			t.Fatalf("CreateMemory failed: %v", err)
		}
	}

	memories, err := mgr.GetMemories(ctx, "user-limit", "agent-limit", 2)
	if err != nil {
		t.Fatalf("GetMemories failed: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(memories))
	}
}

func TestManager_CreateMemoryEmptyInput(t *testing.T) {
	ctx := context.Background()
	store := in_memory.New()
	mgr := memory.NewManager(store)

	if _, err := mgr.CreateMemory(ctx, "", "user-1", "agent-1"); err == nil {
		t.Fatal("expected error for empty input")
	}
}
