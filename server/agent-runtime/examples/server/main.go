package main

import (
	"log"
	stdos "os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os"
)

func main() {
	agents := make(map[string]*agent.Agent)

	// Mock agent that does not need an API key.
	mockModel := mock.New()
	mockAgent, err := agent.New(agent.AgentConfig{
		ID:           "mock-agent",
		Name:         "Mock Agent",
		Instructions: "You are a mock test agent.",
		Model:        mockModel,
	})
	if err != nil {
		log.Fatalf("create mock agent: %v", err)
	}
	agents["mock-agent"] = mockAgent

	// OpenAI agent, only if an API key is available.
	if apiKey := stdos.Getenv("OPENAI_API_KEY"); apiKey != "" {
		openaiModel := openai.NewChat("gpt-4o-mini", apiKey)
		openaiAgent, err := agent.New(agent.AgentConfig{
			ID:           "openai-agent",
			Name:         "OpenAI Agent",
			Instructions: "You are a helpful assistant.",
			Model:        openaiModel,
		})
		if err != nil {
			log.Fatalf("create openai agent: %v", err)
		}
		agents["openai-agent"] = openaiAgent
	}

	agentOS, err := os.New(os.AgentOSConfig{
		Agents: agents,
	})
	if err != nil {
		log.Fatalf("create AgentOS: %v", err)
	}

	port := stdos.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("AgentOS listening on %s", addr)
	if err := agentOS.Serve(addr); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
