package sqlite

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"
)

// applyMigrations keeps the ledger check, migration body, and ledger insert
// under one SQLite writer transaction. This avoids the check-then-begin race
// of generic per-file migration helpers when two processes open a fresh file.
func (d *DB) applyMigrations(ctx context.Context) error {
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agent_runtime_schema_migrations (version BIGINT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
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
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_runtime_schema_migrations WHERE version=?`, version).Scan(&applied); err != nil {
			return err
		}
		if applied != 0 {
			continue
		}
		body, err := fs.ReadFile(migrations, path.Join("migrations", entry.Name()))
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO agent_runtime_schema_migrations(version,applied_at) VALUES(?,?)`, version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
