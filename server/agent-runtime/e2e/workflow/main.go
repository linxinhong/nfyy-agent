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

	wf, err := workflow.New(workflow.WorkflowConfig{
		Name: "e2e-title-and-expand",
		Steps: []workflow.Step{
			{Name: "generate-title", Agent: titleAgent},
			{Name: "expand-content", Agent: expandAgent},
		},
	})
	if err != nil {
		log.Fatalf("create workflow: %v", err)
	}

	out, err := wf.Run(context.Background(), "Artificial Intelligence")
	if err != nil {
		log.Fatalf("run workflow: %v", err)
	}

	if out.Content == "" {
		log.Fatal("workflow returned empty content")
	}
	fmt.Println("workflow output:", out.Content)
}
