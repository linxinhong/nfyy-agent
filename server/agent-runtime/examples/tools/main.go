package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// getWeather is a simple example tool. In a real app this would call an API.
func getWeather(city string) string {
	return fmt.Sprintf("The weather in %s is sunny and 25°C.", city)
}

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

	weatherTool, err := tools.NewFunction("get_weather", "Get the current weather for a city.", getWeather)
	if err != nil {
		log.Fatalf("create tool: %v", err)
	}

	model := openai.NewChat(modelID, apiKey)
	if baseURL != "" {
		model.SetBaseURL(baseURL)
	}
	ag, err := agent.New(agent.AgentConfig{
		Name:         "tool-agent",
		Instructions: "You have access to a get_weather tool. Use it when the user asks about weather.",
		Model:        model,
		Tools:        []tools.Tool{weatherTool},
		MaxToolCalls: 5,
	})
	if err != nil {
		log.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(context.Background(), "What's the weather in Paris?")
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	fmt.Println(out.Content)
}
