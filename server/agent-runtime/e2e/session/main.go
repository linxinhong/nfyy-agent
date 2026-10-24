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
		Name:         "e2e-session",
		Instructions: "You are a helpful assistant. Remember the user's name if provided.",
		Model:        model,
		DB:           store,
		SessionState: map[string]any{"language": "english"},
	})
	if err != nil {
		log.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(context.Background(), "My name is Alice.")
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	fmt.Println("content:", out.Content)
	fmt.Println("session:", out.SessionID)

	state, err := ag.GetSessionState(context.Background(), out.SessionID)
	if err != nil {
		log.Fatalf("GetSessionState: %v", err)
	}
	fmt.Printf("session state: %+v\n", state)

	if err := ag.UpdateSessionState(context.Background(), out.SessionID, map[string]any{"name": "Alice"}); err != nil {
		log.Fatalf("UpdateSessionState: %v", err)
	}

	state, err = ag.GetSessionState(context.Background(), out.SessionID)
	if err != nil {
		log.Fatalf("GetSessionState after update: %v", err)
	}
	fmt.Printf("session state after update: %+v\n", state)
}
