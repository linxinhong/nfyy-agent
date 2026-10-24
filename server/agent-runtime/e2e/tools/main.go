package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/builtin"
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

	calculator, err := builtin.NewCalculatorTool()
	if err != nil {
		log.Fatalf("create calculator: %v", err)
	}

	ag, err := agent.New(agent.AgentConfig{
		Name:         "e2e-tools",
		Instructions: "You are a helpful assistant with a calculator. Use the calculator tool for math.",
		Model:        model,
		Tools: []tools.Tool{
			calculator,
		},
	})
	if err != nil {
		log.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(context.Background(), "What is 135 * 27?")
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	fmt.Println("content:", out.Content)
	fmt.Println("tool calls:", len(out.ToolCalls))
}
