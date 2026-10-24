// Package in_memory provides an in-memory implementation of the db.DB interface.
package in_memory

import (
	"context"
	"fmt"
	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"time"
)

func (db *DB) CreateMemory(_ context.Context, m *memory.UserMemory) error {
	if m == nil {
		return fmt.Errorf("memory is nil")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if m.MemoryID == "" {
		m.MemoryID = generateID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}

	clone, err := cloneMemory(m)
	if err != nil {
		return err
	}
	key := nsKey(m.Namespace, m.MemoryID)
	db.memories[key] = clone
	indexAdd(db.memoryIndex, m.Namespace, m.MemoryID)
	return nil
}

// ReadMemory returns a memory by id. Returns nil, nil if not found.
func (db *DB) ReadMemory(_ context.Context, memoryID string) (*memory.UserMemory, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is empty")
	}

	db.mu.RLock()
	defer db.mu.RUnlock()

	key, ok, err := findKey(db.memoryIndex, memoryID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	m, ok := db.memories[key]
	if !ok {
		return nil, nil
	}
	return cloneMemory(m)
}

// UpdateMemory replaces an existing memory.
func (db *DB) UpdateMemory(_ context.Context, m *memory.UserMemory) error {
	if m == nil {
		return fmt.Errorf("memory is nil")
	}
	if m.MemoryID == "" {
		return fmt.Errorf("memory_id is empty")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if _, ok := db.memories[nsKey(m.Namespace, m.MemoryID)]; !ok {
		return fmt.Errorf("memory not found: %s", m.MemoryID)
	}

	clone, err := cloneMemory(m)
	if err != nil {
		return err
	}
	if clone.UpdatedAt.IsZero() {
		clone.UpdatedAt = time.Now().UTC()
	}
	key := nsKey(m.Namespace, m.MemoryID)
	db.memories[key] = clone
	indexAdd(db.memoryIndex, m.Namespace, m.MemoryID)
	return nil
}

// GetMemories returns memories for a user/agent ordered by most recently created.
func (db *DB) GetMemories(_ context.Context, userID, agentID string, limit int) ([]*memory.UserMemory, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var result []*memory.UserMemory
	for _, m := range db.memories {
		if userID != "" && m.UserID != userID {
			continue
		}
		if agentID != "" && m.AgentID != agentID {
			continue
		}
		result = append(result, m)
	}

	for i := 0; i < len(result)-1; i++ {
		for j := i + 1; j < len(result); j++ {
			if result[i].CreatedAt.Before(result[j].CreatedAt) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	if limit > 0 && limit < len(result) {
		result = result[:limit]
	}

	return cloneMemories(result)
}

// DeleteMemory removes a memory by id.
func (db *DB) DeleteMemory(_ context.Context, memoryID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	key, ok, err := findKey(db.memoryIndex, memoryID)
	if err != nil {
		return err
	}
	if !ok {
		return dbpkg.ErrNotFound
	}
	delete(db.memories, key)
	delete(db.memoryIndex, memoryID)
	return nil
}

// Ping always succeeds for the in-memory store.
