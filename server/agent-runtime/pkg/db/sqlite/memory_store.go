// Package sqlite provides a SQLite-backed implementation of the db.DB interface.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"time"
)

func (d *DB) CreateMemory(ctx context.Context, m *memory.UserMemory) error {
	if m == nil {
		return fmt.Errorf("memory is nil")
	}
	if m.MemoryID == "" {
		m.MemoryID = generateID()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}

	topicsJSON, err := toJSON(m.Topics)
	if err != nil {
		return err
	}

	_, err = d.db.ExecContext(ctx,
		`INSERT INTO memories (memory_id, memory, topics, user_id, agent_id, team_id, input, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.MemoryID, m.Memory, topicsJSON, m.UserID, m.AgentID, m.TeamID, m.Input,
		m.CreatedAt.Format(time.RFC3339Nano), m.UpdatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert memory: %w", err)
	}
	return nil
}

// GetMemories returns memories for a user/agent ordered by most recently created.
func (d *DB) GetMemories(ctx context.Context, userID, agentID string, limit int) ([]*memory.UserMemory, error) {
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = d.db.QueryContext(ctx,
			`SELECT memory_id, memory, topics, user_id, agent_id, team_id, input, created_at, updated_at
			 FROM memories WHERE user_id = ? AND agent_id = ? ORDER BY created_at DESC LIMIT ?`,
			userID, agentID, limit)
	} else {
		rows, err = d.db.QueryContext(ctx,
			`SELECT memory_id, memory, topics, user_id, agent_id, team_id, input, created_at, updated_at
			 FROM memories WHERE user_id = ? AND agent_id = ? ORDER BY created_at DESC`,
			userID, agentID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*memory.UserMemory
	for rows.Next() {
		m, err := d.scanMemory(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// ReadMemory loads a memory by id.
func (d *DB) ReadMemory(ctx context.Context, memoryID string) (*memory.UserMemory, error) {
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is empty")
	}

	row := d.db.QueryRowContext(ctx,
		`SELECT memory_id, memory, topics, user_id, agent_id, team_id, input, created_at, updated_at
		 FROM memories WHERE memory_id = ?`, memoryID)

	m, err := d.scanMemory(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// UpdateMemory replaces an existing memory.
func (d *DB) UpdateMemory(ctx context.Context, m *memory.UserMemory) error {
	if m == nil {
		return fmt.Errorf("memory is nil")
	}
	if m.MemoryID == "" {
		return fmt.Errorf("memory_id is empty")
	}

	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = time.Now().UTC()
	}

	topicsJSON, err := toJSON(m.Topics)
	if err != nil {
		return err
	}

	res, err := d.db.ExecContext(ctx,
		`UPDATE memories SET memory = ?, topics = ?, user_id = ?, agent_id = ?, team_id = ?, input = ?, updated_at = ?
		 WHERE memory_id = ?`,
		m.Memory, topicsJSON, m.UserID, m.AgentID, m.TeamID, m.Input,
		m.UpdatedAt.Format(time.RFC3339Nano), m.MemoryID,
	)
	if err != nil {
		return fmt.Errorf("update memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("memory not found: %s", m.MemoryID)
	}
	return nil
}

// DeleteMemory removes a memory by id.
func (d *DB) DeleteMemory(ctx context.Context, memoryID string) error {
	if memoryID == "" {
		return fmt.Errorf("memory_id is empty")
	}
	res, err := d.db.ExecContext(ctx, `DELETE FROM memories WHERE memory_id = ?`, memoryID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

// Ping verifies the database connection.
func (d *DB) scanMemory(scanner interface {
	Scan(dest ...any) error
}) (*memory.UserMemory, error) {
	var m memory.UserMemory
	var topicsRaw sql.NullString
	var createdAtStr, updatedAtStr string

	if err := scanner.Scan(&m.MemoryID, &m.Memory, &topicsRaw, &m.UserID, &m.AgentID, &m.TeamID, &m.Input, &createdAtStr, &updatedAtStr); err != nil {
		return nil, err
	}

	if topicsRaw.Valid && topicsRaw.String != "" {
		if err := fromJSON(topicsRaw.String, &m.Topics); err != nil {
			return nil, err
		}
	}

	createdAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return nil, err
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtStr)
	if err != nil {
		return nil, err
	}
	m.CreatedAt = createdAt
	m.UpdatedAt = updatedAt
	return &m, nil
}
