package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// logger is a simple pre/post run hook.
type logger struct{}

func (logger) PreRun(ctx context.Context, input *session.RunInput) error {
	fmt.Printf("[pre-run] input: %q\n", input.Message)
	return nil
}

func (logger) PostRun(ctx context.Context, output *session.RunOutput) error {
	fmt.Printf("[post-run] status: %s, content length: %d\n", output.Status, len(output.Content))
	return nil
}

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY is required")
	}

	model := openai.NewChat("gpt-4o-mini", apiKey)
	ag, err := agent.New(agent.AgentConfig{
		Name:         "hooks-agent",
		Instructions: "You are a helpful assistant.",
		Model:        model,
		Hooks: &agent.Hooks{
			PreRun:  logger{},
			PostRun: logger{},
		},
	})
	if err != nil {
		log.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(context.Background(), "Say hi in one sentence.")
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	fmt.Println(out.Content)
}
