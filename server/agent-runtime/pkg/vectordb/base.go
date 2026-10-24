// Package vectordb defines the vector database abstraction used by knowledge bases.
package vectordb

import "context"

// Document is a chunk of knowledge with optional embedding and metadata.
type Document struct {
	ID        string         `json:"id"`
	Content   string         `json:"content"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Embedding []float32      `json:"embedding,omitempty"`
}

// VectorDb is the abstract vector store.
type VectorDb interface {
	// Create initializes the collection/table.
	Create(ctx context.Context) error
	// Insert stores documents (embeddings may be computed by the implementation).
	Insert(ctx context.Context, docs []Document) error
	// Search returns the most similar documents for the query embedding.
	Search(ctx context.Context, query []float32, limit int, filters map[string]any) ([]Document, error)
	// Delete removes the collection/table and all documents.
	Delete(ctx context.Context) error
	// Exists reports whether the collection/table exists.
	Exists(ctx context.Context) (bool, error)
}
