package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
)

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	model := openai.NewChat("gpt-4o-mini", apiKey)
	store := in_memory.New()
	mgr := memory.NewManager(store)

	ag, err := agent.New(agent.AgentConfig{
		Name:         "memory-agent",
		Instructions: "You are a helpful assistant that remembers facts about the user.",
		Model:        model,
		DB:           store,
		Memory:       mgr,
		UserID:       "demo-user",
	})
	if err != nil {
		log.Fatalf("create agent: %v", err)
	}

	_, err = ag.Run(context.Background(), "My favorite color is blue.")
	if err != nil {
		log.Fatalf("run 1: %v", err)
	}

	out, err := ag.Run(context.Background(), "What is my favorite color?")
	if err != nil {
		log.Fatalf("run 2: %v", err)
	}

	fmt.Println(out.Content)
}
