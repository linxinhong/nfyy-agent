package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestUnconfiguredAndInvalidDatabase(t *testing.T) {
	store, err := Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Ready(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	if _, err := store.Migrate(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), "postgres://user:secret@localhost:invalid/database")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe configuration error: %v", err)
	}
}

func TestMigrations(t *testing.T) {
	dsn := os.Getenv("YUANPU_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YUANPU_TEST_DATABASE_URL to run real PostgreSQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("test database unavailable")
	}
	defer admin.Close(context.Background())
	schema := fmt.Sprintf("yuanpu_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	// Do not fall back to public: the Compose service has already migrated it.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{pool: pool}
	defer store.Close()
	if _, err := store.Ready(ctx); !errors.Is(err, ErrMigrations) {
		t.Fatalf("unmigrated database appeared ready: %v", err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			version, err := store.Migrate(ctx)
			if err == nil && version != 4 {
				err = fmt.Errorf("unexpected migration version %d", version)
			}
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent migration was not idempotent: %v", err)
		}
	}
	if version, err := store.Ready(ctx); err != nil || version != 4 {
		t.Fatalf("database not ready after migration: %d %v", version, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE yuanpu_schema_migrations SET checksum = 'tampered' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err == nil {
		t.Fatal("edited migration history accepted")
	}
	if _, err := store.Ready(ctx); !errors.Is(err, ErrMigrations) {
		t.Fatal("edited history remained ready")
	}
	if _, err := pool.Exec(ctx, "INSERT INTO yuanpu_schema_migrations (version, name, checksum) VALUES (999, 'future', 'future')"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); !errors.Is(err, ErrMigrations) {
		t.Fatal("newer schema accepted by older binary")
	}
}
