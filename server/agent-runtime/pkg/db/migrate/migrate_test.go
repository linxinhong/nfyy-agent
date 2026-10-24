package migrate_test

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/migrate"
	_ "modernc.org/sqlite"
)

func TestApplyRunsOrderedMigrationsOnce(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	files := fstest.MapFS{
		"migrations/001_create.sql": {Data: []byte(`CREATE TABLE items(id INTEGER PRIMARY KEY, value TEXT);`)},
		"migrations/002_seed.sql":   {Data: []byte(`INSERT INTO items(id,value) VALUES(1,'one');`)},
	}
	if err := migrate.Apply(context.Background(), database, files, "migrations", "sqlite"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := migrate.Apply(context.Background(), database, files, "migrations", "sqlite"); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	var items, versions int
	_ = database.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&items)
	_ = database.QueryRow(`SELECT COUNT(*) FROM agent_runtime_schema_migrations`).Scan(&versions)
	if items != 1 || versions != 2 {
		t.Fatalf("items=%d versions=%d", items, versions)
	}
}
