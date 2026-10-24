package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
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

	titleModel := openai.NewChat(modelID, apiKey)
	if baseURL != "" {
		titleModel.SetBaseURL(baseURL)
	}
	expandModel := openai.NewChat(modelID, apiKey)
	if baseURL != "" {
		expandModel.SetBaseURL(baseURL)
	}

	titleAgent, err := agent.New(agent.AgentConfig{
		Name:         "title-agent",
		Instructions: "You are a title generator. Respond with a short, catchy title for the user's topic. Only output the title.",
		Model:        titleModel,
	})
	if err != nil {
		log.Fatalf("create title agent: %v", err)
	}

	expandAgent, err := agent.New(agent.AgentConfig{
		Name:         "expand-agent",
		Instructions: "You are a content expander. Take the provided title and write one engaging paragraph about the topic.",
		Model:        expandModel,
	})
	if err != nil {
		log.Fatalf("create expand agent: %v", err)
	}

	registry := workflow.NewRegistry()
	registry.RegisterExecutor("title-agent", workflow.AgentExecutor(titleAgent))
	registry.RegisterExecutor("expand-agent", workflow.AgentExecutor(expandAgent))
	wf, err := workflow.New(workflow.Config{
		Definition: workflow.Definition{
			ID:      "title-and-expand",
			Name:    "Title and expand",
			Version: "v1",
			Root: workflow.Sequence("root",
				workflow.ExecutorNode("generate-title", "title-agent"),
				workflow.ExecutorNode("expand-content", "expand-agent"),
			),
		},
		Registry: registry,
	})
	if err != nil {
		log.Fatalf("create workflow: %v", err)
	}

	out, err := wf.Run(context.Background(), workflow.RunRequest{Input: "Artificial Intelligence"})
	if err != nil {
		log.Fatalf("run workflow: %v", err)
	}

	fmt.Println(out.Content)
}
