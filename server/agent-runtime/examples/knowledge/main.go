package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/knowledge"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
	"github.com/linxinhong/xingding-agent-runtime/pkg/vectordb"
)

var _ knowledge.Knowledge = (*staticKnowledge)(nil)

// staticKnowledge is a tiny in-memory knowledge base for demonstration.
type staticKnowledge struct {
	docs []vectordb.Document
}

func (s *staticKnowledge) Search(_ context.Context, query string, limit int) ([]vectordb.Document, error) {
	query = strings.ToLower(query)
	var results []vectordb.Document
	for _, d := range s.docs {
		if strings.Contains(strings.ToLower(d.Content), query) {
			results = append(results, d)
			if len(results) >= limit {
				break
			}
		}
	}
	return results, nil
}

func (s *staticKnowledge) GetContent(_ context.Context, docID string) (string, error) {
	for _, d := range s.docs {
		if d.ID == docID {
			return d.Content, nil
		}
	}
	return "", fmt.Errorf("document not found: %s", docID)
}

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	kb := &staticKnowledge{
		docs: []vectordb.Document{
			{ID: "1", Content: "Agno is an agent framework written in Python."},
			{ID: "2", Content: "The Go port aims to bring the same core concepts to Go."},
		},
	}

	model := openai.NewChat("gpt-4o-mini", apiKey)
	ag, err := agent.New(agent.AgentConfig{
		Name:         "knowledge-agent",
		Instructions: "Answer questions using the knowledge base. If the answer is not in the knowledge base, say so.",
		Model:        model,
		Knowledge:    kb,
		MaxToolCalls: 3,
	})
	if err != nil {
		log.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(context.Background(), "What is Agno?")
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	fmt.Println(out.Content)
}
