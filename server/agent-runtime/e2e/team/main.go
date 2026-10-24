package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
	"github.com/linxinhong/xingding-agent-runtime/pkg/team"
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

	leaderModel := openai.NewChat(modelID, apiKey)
	if baseURL != "" {
		leaderModel.SetBaseURL(baseURL)
	}

	researcherModel := openai.NewChat(modelID, apiKey)
	if baseURL != "" {
		researcherModel.SetBaseURL(baseURL)
	}
	writerModel := openai.NewChat(modelID, apiKey)
	if baseURL != "" {
		writerModel.SetBaseURL(baseURL)
	}

	store := in_memory.New()

	researcher, err := agent.New(agent.AgentConfig{
		Name:         "researcher",
		Instructions: "You are a research assistant. Provide 2-3 concise facts about the user's topic.",
		Model:        researcherModel,
		DB:           store,
	})
	if err != nil {
		log.Fatalf("create researcher: %v", err)
	}

	writer, err := agent.New(agent.AgentConfig{
		Name:         "writer",
		Instructions: "You are a writer. Summarize the provided facts into one polished sentence.",
		Model:        writerModel,
		DB:           store,
	})
	if err != nil {
		log.Fatalf("create writer: %v", err)
	}

	t, err := team.New(team.TeamConfig{
		Name:         "e2e-research-team",
		Instructions: "You coordinate a researcher and a writer. Delegate research first, then ask the writer to summarize the facts into one sentence.",
		Model:        leaderModel,
		Members:      []*agent.Agent{researcher, writer},
		DB:           store,
	})
	if err != nil {
		log.Fatalf("create team: %v", err)
	}

	out, err := t.Run(context.Background(), "Tell me about Go programming language.")
	if err != nil {
		log.Fatalf("run team: %v", err)
	}

	if out.Content == "" {
		log.Fatal("team returned empty content")
	}
	fmt.Println("team output:", out.Content)
	fmt.Println("session:", out.SessionID)
}
