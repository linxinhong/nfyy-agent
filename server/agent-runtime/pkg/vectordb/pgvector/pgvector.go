// Package pgvector provides a PostgreSQL + pgvector implementation of vectordb.VectorDb.
package pgvector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	_ "github.com/lib/pq"
	"github.com/linxinhong/xingding-agent-runtime/pkg/vectordb"
)

// DistanceMetric selects the pgvector distance operator used for search.
type DistanceMetric string

const (
	// Cosine uses the <=> operator (default).
	Cosine DistanceMetric = "cosine"
	// Euclidean uses the <-> operator.
	Euclidean DistanceMetric = "euclidean"
	// InnerProduct uses the <#> operator.
	InnerProduct DistanceMetric = "inner_product"
)

// DB is a pgvector-backed VectorDb implementation.
type DB struct {
	db        *sql.DB
	tableName string
	dims      int
	metric    DistanceMetric
}

// compile-time interface check.
var _ vectordb.VectorDb = (*DB)(nil)

// New creates a new pgvector vector store.
// connString is a lib/pq connection string. tableName is the collection table.
// dims is the vector dimension. metric defaults to Cosine when empty.
func New(connString, tableName string, dims int, metric DistanceMetric) (*DB, error) {
	if connString == "" {
		return nil, fmt.Errorf("connString is empty")
	}
	if tableName == "" {
		return nil, fmt.Errorf("tableName is empty")
	}
	if dims <= 0 {
		return nil, fmt.Errorf("dims must be positive")
	}
	if metric == "" {
		metric = Cosine
	}

	db, err := sql.Open("postgres", connString)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	d := &DB{
		db:        db,
		tableName: quoteIdentifier(tableName),
		dims:      dims,
		metric:    metric,
	}
	return d, nil
}

// Create enables the pgvector extension and creates the collection table.
func (d *DB) Create(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("create vector extension: %w", err)
	}

	sql := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			metadata JSONB,
			embedding vector(%d)
		)`,
		d.tableName, d.dims,
	)
	if _, err := d.db.ExecContext(ctx, sql); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	return nil
}

// Insert stores documents in the collection. Embeddings are expected to be populated.
func (d *DB) Insert(ctx context.Context, docs []vectordb.Document) error {
	if len(docs) == 0 {
		return nil
	}

	sql := fmt.Sprintf(
		`INSERT INTO %s (id, content, metadata, embedding)
		 VALUES ($1, $2, $3, $4::vector)
		 ON CONFLICT (id) DO UPDATE SET
			content = EXCLUDED.content,
			metadata = EXCLUDED.metadata,
			embedding = EXCLUDED.embedding`,
		d.tableName,
	)

	for _, doc := range docs {
		if doc.ID == "" {
			return fmt.Errorf("document id is empty")
		}
		if len(doc.Embedding) != d.dims {
			return fmt.Errorf("document %s embedding dimension %d does not match collection dimension %d", doc.ID, len(doc.Embedding), d.dims)
		}

		metadataJSON, err := toJSON(doc.Metadata)
		if err != nil {
			return err
		}

		if _, err := d.db.ExecContext(ctx, sql, doc.ID, doc.Content, metadataJSON, vectorString(doc.Embedding)); err != nil {
			return fmt.Errorf("insert document %s: %w", doc.ID, err)
		}
	}
	return nil
}

// Search returns the closest documents to the query embedding.
func (d *DB) Search(ctx context.Context, query []float32, limit int, filters map[string]any) ([]vectordb.Document, error) {
	if len(query) != d.dims {
		return nil, fmt.Errorf("query embedding dimension %d does not match collection dimension %d", len(query), d.dims)
	}
	if limit <= 0 {
		limit = 5
	}

	operator := d.operator()
	whereClause := ""
	args := []any{vectorString(query), limit}
	if len(filters) > 0 {
		metadataJSON, err := toJSON(filters)
		if err != nil {
			return nil, err
		}
		whereClause = "WHERE metadata @> $3::jsonb"
		args = append(args, metadataJSON)
	}

	sql := fmt.Sprintf(
		`SELECT id, content, metadata FROM %s %s ORDER BY embedding %s $1::vector LIMIT $2`,
		d.tableName, whereClause, operator,
	)

	rows, err := d.db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search documents: %w", err)
	}
	defer rows.Close()

	var docs []vectordb.Document
	for rows.Next() {
		var doc vectordb.Document
		var metadataRaw []byte
		if err := rows.Scan(&doc.ID, &doc.Content, &metadataRaw); err != nil {
			return nil, err
		}
		if len(metadataRaw) > 0 {
			_ = fromJSON(string(metadataRaw), &doc.Metadata)
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return docs, nil
}

// Delete drops the collection table.
func (d *DB) Delete(ctx context.Context) error {
	sql := fmt.Sprintf("DROP TABLE IF EXISTS %s", d.tableName)
	_, err := d.db.ExecContext(ctx, sql)
	return err
}

// Exists reports whether the collection table exists.
func (d *DB) Exists(ctx context.Context) (bool, error) {
	var exists bool
	sql := `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = $1
	)`
	rawName := strings.Trim(d.tableName, `"`)
	if err := d.db.QueryRowContext(ctx, sql, rawName).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// Close releases the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) operator() string {
	switch d.metric {
	case Euclidean:
		return "<->"
	case InnerProduct:
		return "<#>"
	case Cosine, "":
		return "<=>"
	default:
		return "<=>"
	}
}

func vectorString(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func quoteIdentifier(name string) string {
	// Minimal quoting for table names; sufficient for placeholder usage.
	return `"` + strings.ReplaceAll(name, `"`, `\"`) + `"`
}

func toJSON(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func fromJSON(s string, v any) error {
	if s == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), v)
}
