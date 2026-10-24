package pgvector

import (
	"context"
	"os"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/vectordb"
)

func skipNoDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_PGVECTOR_DSN")
	if dsn == "" {
		t.Skip("TEST_PGVECTOR_DSN not set; skipping pgvector integration test")
	}
	return dsn
}

func TestDB_CreateAndExists(t *testing.T) {
	dsn := skipNoDSN(t)
	ctx := context.Background()

	db, err := New(dsn, "agno_test_docs", 3, Cosine)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer db.Close()

	if err := db.Delete(ctx); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if err := db.Create(ctx); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	exists, err := db.Exists(ctx)
	if err != nil {
		t.Fatalf("Exists failed: %v", err)
	}
	if !exists {
		t.Fatal("expected table to exist after Create")
	}

	if err := db.Delete(ctx); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
}

func TestDB_InsertAndSearch(t *testing.T) {
	dsn := skipNoDSN(t)
	ctx := context.Background()

	db, err := New(dsn, "agno_test_search", 3, Cosine)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer db.Close()

	_ = db.Delete(ctx)
	if err := db.Create(ctx); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	docs := []vectordb.Document{
		{
			ID:        "doc-1",
			Content:   "hello world",
			Embedding: []float32{1, 0, 0},
			Metadata:  map[string]any{"group": "a"},
		},
		{
			ID:        "doc-2",
			Content:   "goodbye world",
			Embedding: []float32{0, 1, 0},
			Metadata:  map[string]any{"group": "b"},
		},
	}

	if err := db.Insert(ctx, docs); err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	results, err := db.Search(ctx, []float32{1, 0, 0}, 1, nil)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ID != "doc-1" {
		t.Fatalf("expected doc-1, got %s", results[0].ID)
	}

	filtered, err := db.Search(ctx, []float32{1, 0, 0}, 10, map[string]any{"group": "b"})
	if err != nil {
		t.Fatalf("Search with filters failed: %v", err)
	}
	if len(filtered) != 1 || filtered[0].ID != "doc-2" {
		t.Fatalf("unexpected filtered results: %+v", filtered)
	}

	_ = db.Delete(ctx)
}
