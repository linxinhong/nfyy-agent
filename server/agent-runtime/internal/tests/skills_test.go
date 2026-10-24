package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/skills"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

const skillBody = "Skill body content that must only appear after load_skill."

// TestE2ESkillProgressiveDisclosure verifies the core contract end to end:
// the initial system context carries only the skill catalog (name +
// description), and load_skill fetches the body on demand, after which the
// tool result reaches the model.
func TestE2ESkillProgressiveDisclosure(t *testing.T) {
	skillDir := t.TempDir()
	skillPath := filepath.Join(skillDir, "SKILL.md")
	md := "---\nname: demo-skill\ndescription: Demonstrates a skill\n---\n\n" + skillBody + "\n"
	if err := os.WriteFile(skillPath, []byte(md), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	skill, err := skills.Load(skillPath)
	if err != nil {
		t.Fatalf("load skill: %v", err)
	}

	fake := NewFakeChat(
		&models.ModelResponse{ToolCalls: []models.ToolCall{{
			ID: "call-1", Type: "function",
			Function: models.FunctionCall{Name: "load_skill", Arguments: `{"name":"demo-skill"}`},
		}}},
		&models.ModelResponse{Content: "skill loaded"},
	)
	srv := NewServer(t,
		WithFakeChat(fake),
		WithTools([]tools.Tool{&agent.LoadSkillTool{Skills: []*skills.Skill{skill}}}),
		WithContextProviders([]agent.ContextProvider{&agent.SkillContextProvider{Skills: []*skills.Skill{skill}}}),
	)

	var out session.RunOutput
	status, err := srv.PostJSON("/agents/"+srv.AgentID()+"/runs", map[string]any{
		"message": "use the skill",
	}, &out)
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200 (error: %q)", status, out.Error)
	}
	if out.Content != "skill loaded" {
		t.Fatalf("content = %q, want skill loaded", out.Content)
	}

	received := fake.Received()
	if len(received) < 2 {
		t.Fatalf("model calls = %d, want >= 2 (catalog round + load_skill result round)", len(received))
	}

	// Round 1: system context carries the catalog but NOT the body.
	firstSystem := systemText(received[0])
	if !strings.Contains(firstSystem, "demo-skill: Demonstrates a skill") {
		t.Fatalf("catalog missing from initial system: %q", firstSystem)
	}
	if strings.Contains(firstSystem, skillBody) {
		t.Fatalf("skill body leaked into initial system context (progressive disclosure violated): %q", firstSystem)
	}

	// Round 2: the load_skill tool result (body) reaches the model.
	secondAll := allContent(received[1])
	if !strings.Contains(secondAll, skillBody) {
		t.Fatalf("load_skill body missing from second-round messages: %q", secondAll)
	}
}

func systemText(messages []models.Message) string {
	var b strings.Builder
	for _, m := range messages {
		if m.Role == models.RoleSystem {
			b.WriteString(m.Content)
		}
	}
	return b.String()
}

func allContent(messages []models.Message) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}
