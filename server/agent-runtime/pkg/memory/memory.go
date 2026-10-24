// Package memory provides user memory management for agents.
package memory

import (
	"context"
	"time"
)

// UserMemory is a single fact remembered about a user.
type UserMemory struct {
	MemoryID  string    `json:"memory_id"`
	Memory    string    `json:"memory"`
	Topics    []string  `json:"topics,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	TeamID    string    `json:"team_id,omitempty"`
	Input     string    `json:"input,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Manager defines how agents create and retrieve memories.
type Manager interface {
	CreateMemory(ctx context.Context, input string, userID, agentID string) (*UserMemory, error)
	GetMemories(ctx context.Context, userID, agentID string, limit int) ([]*UserMemory, error)
}
