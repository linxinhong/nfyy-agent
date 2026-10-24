package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
)

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	modelID := os.Getenv("MODEL")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}
	if modelID == "" {
		modelID = "gpt-4o-mini"
	}

	model := openai.NewChat(modelID, apiKey)
	if baseURL != "" {
		model.SetBaseURL(baseURL)
	}
	store := in_memory.New()

	ag, err := agent.New(agent.AgentConfig{
		Name:         "e2e-basic",
		Instructions: "You are a helpful assistant.",
		Model:        model,
		DB:           store,
	})
	if err != nil {
		log.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(context.Background(), "Say hello in one sentence.")
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	fmt.Println("content:", out.Content)
	fmt.Println("session:", out.SessionID)
}
