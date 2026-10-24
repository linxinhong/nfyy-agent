package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	agentruntime "github.com/linxinhong/xingding-agent-runtime/pkg/runtime"
)

// This example boots an AgentOS whose agent has the code-reviewer skill
// available. The skill body is NOT injected into the system context up
// front: the model sees only the catalog and calls load_skill to fetch the
// body on demand (progressive disclosure).
func main() {
	root, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	skillsDir := filepath.Join(root, "examples", "skills")

	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion,
		ID:            "skills-demo", Version: "v1", Name: "Skills Demo",
		Agents: []agentruntime.AgentSpec{{
			ID:           "reviewer",
			Name:         "Reviewer",
			ModelRef:     "chat",
			Instructions: "You are a Go code reviewer. Use the load_skill tool to load the code-reviewer skill when reviewing code.",
			SkillDirs:    []string{filepath.Join(skillsDir, "code-reviewer")},
		}},
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		Models: map[string]models.Model{"chat": mock.New()},
	})
	if err != nil {
		log.Fatalf("build project: %v", err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("AgentOS listening on :%s", port)
	if err := project.AgentOS().Serve(":" + port); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
