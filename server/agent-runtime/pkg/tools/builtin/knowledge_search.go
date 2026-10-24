// Package builtin provides built-in tools available to agents, teams, and workflows.
package builtin

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// KnowledgeSearch queries the knowledge base attached to the run context.
func KnowledgeSearch(ctx context.Context, query string, limit int) (string, error) {
	rc, ok := RunContextFromContext(ctx)
	if !ok || rc.Knowledge == nil {
		return "", errors.New("no knowledge base available")
	}
	if limit <= 0 {
		limit = 5
	}
	docs, err := rc.Knowledge.Search(ctx, query, limit)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(map[string]any{
		"query": query,
		"limit": limit,
		"docs":  docs,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// NewKnowledgeSearchTool returns a Tool wrapping KnowledgeSearch.
func NewKnowledgeSearchTool() (*tools.Function, error) {
	return tools.NewFunction("knowledge_search", "Search the knowledge base for relevant documents.", KnowledgeSearch)
}
