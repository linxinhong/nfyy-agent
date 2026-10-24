// Package migrate applies ordered embedded SQL migrations.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"
)

// Apply executes unapplied NN_name.sql files in lexical order. Each file and
// its ledger write commit atomically.
func Apply(ctx context.Context, database *sql.DB, files fs.FS, dir, dialect string) error {
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agent_runtime_schema_migrations (version BIGINT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return fmt.Errorf("invalid migration name %q", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return fmt.Errorf("invalid migration version %q", entry.Name())
		}
		var applied int
		first, second := "?", "?"
		if dialect == "postgres" {
			first, second = "$1", "$2"
		}
		query := `SELECT COUNT(*) FROM agent_runtime_schema_migrations WHERE version = ` + first
		if err := database.QueryRowContext(ctx, query, version).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		body, err := fs.ReadFile(files, path.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			insert := `INSERT INTO agent_runtime_schema_migrations(version, applied_at) VALUES (` + first + `,` + second + `)`
			_, err = tx.ExecContext(ctx, insert, version, time.Now().UTC().Format(time.RFC3339Nano))
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
