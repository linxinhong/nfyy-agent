package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
)

type Weather struct {
	City        string  `json:"city"`
	Temperature float64 `json:"temperature"`
	Unit        string  `json:"unit"`
}

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
	ag, err := agent.New(agent.AgentConfig{
		Name:         "e2e-structured",
		Instructions: "You are a weather assistant. Always respond with valid JSON matching the requested schema. Include all required fields.",
		Model:        model,
		OutputSchema: Weather{},
	})
	if err != nil {
		log.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(context.Background(), "What is the weather in Tokyo?")
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	weather, ok := out.Parsed.(Weather)
	if !ok {
		log.Fatalf("unexpected parsed type: %T", out.Parsed)
	}
	if weather.City == "" {
		log.Fatalf("structured output missing city: %+v", weather)
	}
	fmt.Printf("Weather in %s: %.1f%s\n", weather.City, weather.Temperature, weather.Unit)
}
