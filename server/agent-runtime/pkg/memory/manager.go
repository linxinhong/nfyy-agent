// Package memory provides a concrete memory.Manager implementation backed by a memory store.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// DB is the minimal storage interface required by Manager.
// The db.DB interface from pkg/db satisfies this contract.
type DB interface {
	CreateMemory(ctx context.Context, m *UserMemory) error
	GetMemories(ctx context.Context, userID, agentID string, limit int) ([]*UserMemory, error)
}

// manager implements memory.Manager using a DB backend.
// For the MVP it stores the raw input as the memory without LLM summarization.
type manager struct {
	db DB
}

// NewManager returns a memory.Manager backed by the provided store.
// Any implementation of db.DB is accepted because it satisfies the DB interface.
func NewManager(store DB) Manager {
	return &manager{db: store}
}

// CreateMemory stores the raw input as a memory.
func (m *manager) CreateMemory(ctx context.Context, input, userID, agentID string) (*UserMemory, error) {
	if input == "" {
		return nil, fmt.Errorf("input is empty")
	}

	now := time.Now().UTC()
	mem := &UserMemory{
		MemoryID:  generateID(),
		Memory:    input,
		Input:     input,
		UserID:    userID,
		AgentID:   agentID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := m.db.CreateMemory(ctx, mem); err != nil {
		return nil, fmt.Errorf("create memory: %w", err)
	}
	return mem, nil
}

// GetMemories retrieves memories for a user and agent.
func (m *manager) GetMemories(ctx context.Context, userID, agentID string, limit int) ([]*UserMemory, error) {
	return m.db.GetMemories(ctx, userID, agentID, limit)
}

func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
