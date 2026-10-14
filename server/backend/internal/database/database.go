package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuanpu.dev/server/migrations"
)

var (
	ErrNotConfigured = errors.New("database is not configured")
	ErrUnavailable   = errors.New("database is unavailable")
	ErrMigrations    = errors.New("database migrations are not current")
)

type Store struct {
	pool *pgxpool.Pool
}

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

func Open(ctx context.Context, url string) (*Store, error) {
	if strings.TrimSpace(url) == "" {
		return &Store{}, nil
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx parse errors may contain credentials from the DSN.
		return nil, errors.New("invalid YUANPU_DATABASE_URL")
	}
	cfg.MaxConns = 10
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, ErrUnavailable
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Pool is shared with the server's business repositories; only internal
// packages receive it, and migrations remain owned by Store.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func embeddedMigrations() ([]migration, error) {
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		return nil, err
	}
	items := make([]migration, 0, len(names))
	for i, name := range names {
		prefix, _, _ := strings.Cut(name, "_")
		version, err := strconv.Atoi(prefix)
		if err != nil || version != i+1 {
			return nil, fmt.Errorf("migration versions must be contiguous: %s", name)
		}
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		items = append(items, migration{version, name, string(body), hex.EncodeToString(sum[:])})
	}
	return items, nil
}

func (s *Store) Ready(ctx context.Context) (int, error) {
	if s.pool == nil {
		return 0, ErrNotConfigured
	}
	if err := s.pool.Ping(ctx); err != nil {
		return 0, ErrUnavailable
	}
	items, err := embeddedMigrations()
	if err != nil {
		return 0, ErrMigrations
	}
	rows, err := s.pool.Query(ctx, "SELECT version, checksum FROM yuanpu_schema_migrations ORDER BY version")
	if err != nil {
		return 0, ErrMigrations
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil || index >= len(items) || version != items[index].version || checksum != items[index].checksum {
			return 0, ErrMigrations
		}
		index++
	}
	if rows.Err() != nil || index != len(items) {
		return 0, ErrMigrations
	}
	return len(items), nil
}

// Migrate is explicit: serving HTTP never performs DDL. The transaction-level
// advisory lock serializes concurrent installers; checksums reject edited history.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	if s.pool == nil {
		return 0, ErrNotConfigured
	}
	items, err := embeddedMigrations()
	if err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(796873210)"); err != nil {
		return 0, errors.New("could not acquire migration lock")
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS yuanpu_schema_migrations (
		version integer PRIMARY KEY,
		name text NOT NULL,
		checksum text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return 0, errors.New("could not create migration ledger")
	}
	var latest int
	if err := tx.QueryRow(ctx, "SELECT COALESCE(MAX(version), 0) FROM yuanpu_schema_migrations").Scan(&latest); err != nil || latest > len(items) {
		return 0, ErrMigrations
	}
	for _, item := range items {
		var checksum string
		err := tx.QueryRow(ctx, "SELECT checksum FROM yuanpu_schema_migrations WHERE version = $1", item.version).Scan(&checksum)
		if err == nil {
			if checksum != item.checksum {
				return 0, fmt.Errorf("migration %d checksum mismatch", item.version)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("could not read migration ledger")
		}
		if item.version <= latest {
			return 0, errors.New("migration ledger has a gap")
		}
		if _, err := tx.Exec(ctx, item.sql); err != nil {
			return 0, fmt.Errorf("migration %d failed; check database extension support and migration privileges", item.version)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO yuanpu_schema_migrations (version, name, checksum) VALUES ($1, $2, $3)", item.version, item.name, item.checksum); err != nil {
			return 0, errors.New("could not record migration")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, errors.New("could not commit migrations")
	}
	return len(items), nil
}
