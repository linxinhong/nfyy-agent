// Package knowledge provides RAG-style knowledge retrieval for agents.
package knowledge

import (
	"context"

	"github.com/linxinhong/xingding-agent-runtime/pkg/vectordb"
)

// Knowledge is the agent-facing knowledge interface.
type Knowledge interface {
	// Search retrieves documents matching the query.
	Search(ctx context.Context, query string, limit int) ([]vectordb.Document, error)
	// GetContent returns the full content of a document by id.
	GetContent(ctx context.Context, docID string) (string, error)
}

// VectorKnowledge wraps a VectorDb as Knowledge.
type VectorKnowledge struct {
	Name        string
	VectorDB    vectordb.VectorDb
	Embedder    Embedder
	SearchLimit int
}

// Embedder turns text into an embedding vector.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Search implements Knowledge.
func (vk *VectorKnowledge) Search(ctx context.Context, query string, limit int) ([]vectordb.Document, error) {
	if vk.Embedder == nil {
		return nil, ErrNoEmbedder
	}
	if limit == 0 {
		limit = vk.SearchLimit
		if limit == 0 {
			limit = 5
		}
	}
	embedding, err := vk.Embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	return vk.VectorDB.Search(ctx, embedding, limit, nil)
}

// GetContent implements Knowledge.
func (vk *VectorKnowledge) GetContent(ctx context.Context, docID string) (string, error) {
	return "", ErrNotImplemented
}
