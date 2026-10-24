package main

import (
	"log"
	stdos "os"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	agentruntime "github.com/linxinhong/xingding-agent-runtime/pkg/runtime"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

func main() {
	securityKey := stdos.Getenv("AGENTOS_SECURITY_KEY")
	if securityKey == "" {
		log.Fatal("AGENTOS_SECURITY_KEY is required")
	}
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion,
		ID:            "reporting", Version: "v1", Name: "Reporting Project",
		Agents: []agentruntime.AgentSpec{{
			ID: "researcher", Name: "Researcher", ModelRef: "chat",
			Instructions: "Research the requested subject and return a concise report.",
		}},
		Workflows: []agentruntime.WorkflowSpec{{Definition: workflow.Definition{
			ID: "report", Version: "v1",
			Root: workflow.ExecutorNode("research", agentruntime.AgentExecutorRef("researcher")),
		}}},
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		Models:      map[string]models.Model{"chat": mock.New()},
		SecurityKey: securityKey,
	})
	if err != nil {
		log.Fatalf("build project: %v", err)
	}
	port := stdos.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if err := project.AgentOS().Serve(":" + port); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
