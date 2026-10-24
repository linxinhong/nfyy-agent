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
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	modelID := os.Getenv("MODEL")
	if modelID == "" {
		modelID = "gpt-4o-mini"
	}

	model := openai.NewChat(modelID, apiKey)
	if baseURL != "" {
		model.SetBaseURL(baseURL)
	}
	ag, err := agent.New(agent.AgentConfig{
		Name:         "streaming-agent",
		Instructions: "You are a helpful assistant. Answer in one sentence.",
		Model:        model,
	})
	if err != nil {
		log.Fatalf("create agent: %v", err)
	}

	ch, err := ag.RunStream(context.Background(), "Tell me a short joke.")
	if err != nil {
		log.Fatalf("RunStream: %v", err)
	}

	for ev := range ch {
		switch ev.Type {
		case agent.EventTypeRunStarted:
			fmt.Println("[started]")
		case agent.EventTypeMessage:
			fmt.Print(ev.Content)
		case agent.EventTypeRunCompleted:
			fmt.Println("\n[completed]")
		case agent.EventTypeRunError:
			fmt.Printf("\n[error] %v\n", ev.Err)
		}
	}
}
