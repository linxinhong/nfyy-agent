package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
)

func main() {
	if err := runStream(); err != nil {
		log.Fatal(err)
	}
}

func runStream() error {
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
	ag, err := agent.New(agent.AgentConfig{
		Name:         "e2e-streaming",
		Instructions: "You are a helpful assistant. Answer in one sentence.",
		Model:        model,
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	ch, err := ag.RunStream(context.Background(), "Tell me a short joke.")
	if err != nil {
		return fmt.Errorf("RunStream: %w", err)
	}

	fmt.Println("[stream started]")
	for ev := range ch {
		switch ev.Type {
		case agent.EventTypeRunStarted:
			fmt.Println("[run started]")
		case agent.EventTypeMessage:
			fmt.Print(ev.Content)
		case agent.EventTypeRunCompleted:
			fmt.Println("\n[run completed]")
		case agent.EventTypeRunError:
			fmt.Printf("\n[error] %v\n", ev.Err)
		}
	}
	fmt.Println("[stream ended]")
	return nil
}
