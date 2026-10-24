// Package postgres provides a PostgreSQL-backed implementation of the db.DB interface.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"time"
)

func (d *DB) CreateMemory(ctx context.Context, m *memory.UserMemory) error {
	if m.MemoryID == "" {
		m.MemoryID = uuid.NewString()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.CreatedAt
	}
	raw, e := json.Marshal(m)
	if e != nil {
		return e
	}
	_, e = d.db.ExecContext(ctx, `INSERT INTO agent_runtime_memories(memory_id,user_id,agent_id,payload,created_at,updated_at) VALUES($1,$2,$3,$4::jsonb,$5,$6)`, m.MemoryID, m.UserID, m.AgentID, string(raw), m.CreatedAt, m.UpdatedAt)
	return e
}
func (d *DB) ReadMemory(ctx context.Context, id string) (*memory.UserMemory, error) {
	var raw []byte
	e := d.db.QueryRowContext(ctx, `SELECT payload FROM agent_runtime_memories WHERE memory_id=$1`, id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var m memory.UserMemory
	e = json.Unmarshal(raw, &m)
	return &m, e
}
func (d *DB) UpdateMemory(ctx context.Context, m *memory.UserMemory) error {
	m.UpdatedAt = time.Now().UTC()
	raw, e := json.Marshal(m)
	if e != nil {
		return e
	}
	res, e := d.db.ExecContext(ctx, `UPDATE agent_runtime_memories SET user_id=$1,agent_id=$2,payload=$3::jsonb,updated_at=$4 WHERE memory_id=$5`, m.UserID, m.AgentID, string(raw), m.UpdatedAt, m.MemoryID)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}
func (d *DB) GetMemories(ctx context.Context, userID, agentID string, limit int) ([]*memory.UserMemory, error) {
	if limit <= 0 {
		limit = 2147483647
	}
	rows, e := d.db.QueryContext(ctx, `SELECT payload FROM agent_runtime_memories WHERE ($1='' OR user_id=$1) AND ($2='' OR agent_id=$2) ORDER BY created_at DESC LIMIT $3`, userID, agentID, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []*memory.UserMemory
	for rows.Next() {
		var raw []byte
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		var m memory.UserMemory
		if e = json.Unmarshal(raw, &m); e != nil {
			return nil, e
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}
func (d *DB) DeleteMemory(ctx context.Context, id string) error {
	res, e := d.db.ExecContext(ctx, `DELETE FROM agent_runtime_memories WHERE memory_id=$1`, id)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}
