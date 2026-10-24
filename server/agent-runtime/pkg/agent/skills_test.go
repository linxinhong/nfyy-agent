package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/skills"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

func testSkill(name, description, body string, resources ...string) *skills.Skill {
	return &skills.Skill{Name: name, Description: description, Body: body, Resources: resources, Dir: "/tmp/" + name}
}

func TestSkillContextProviderEmptyIsPassThrough(t *testing.T) {
	provider := &SkillContextProvider{}
	req := ContextRequest{System: []models.Message{{Role: models.RoleSystem, Content: "sys"}}}
	out, err := provider.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(out.System) != 1 || out.System[0].Content != "sys" {
		t.Fatalf("empty provider must pass through: %+v", out.System)
	}
}

func TestSkillContextProviderInjectsCatalogOnly(t *testing.T) {
	provider := &SkillContextProvider{Skills: []*skills.Skill{
		testSkill("alpha", "First skill", "ALPHA SECRET BODY"),
		testSkill("beta", "Second skill", "BETA SECRET BODY"),
	}}
	req := ContextRequest{System: []models.Message{{Role: models.RoleSystem, Content: "sys"}}}
	out, err := provider.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(out.System) != 2 {
		t.Fatalf("system messages = %d, want 2", len(out.System))
	}
	catalog := out.System[1].Content
	if !strings.Contains(catalog, "alpha: First skill") || !strings.Contains(catalog, "beta: Second skill") {
		t.Fatalf("catalog missing entries: %q", catalog)
	}
	if strings.Contains(catalog, "ALPHA SECRET BODY") || strings.Contains(catalog, "BETA SECRET BODY") {
		t.Fatalf("catalog leaked skill bodies (progressive disclosure violated): %q", catalog)
	}
}

func TestLoadSkillToolReturnsBodyAndResources(t *testing.T) {
	tool := &LoadSkillTool{Skills: []*skills.Skill{
		testSkill("alpha", "First skill", "the body", "scripts/run.sh", "templates/p.md"),
	}}
	if tool.Name() != "load_skill" {
		t.Fatalf("name = %q", tool.Name())
	}
	if tool.Approval() != tools.ApprovalNone {
		t.Fatalf("Approval() = %q, want none", tool.Approval())
	}
	out, err := tool.Call(context.Background(), map[string]any{"name": "alpha"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	result, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", out)
	}
	if result["body"] != "the body" {
		t.Fatalf("body = %v", result["body"])
	}
	if result["name"] != "alpha" {
		t.Fatalf("name = %v", result["name"])
	}
	resources, ok := result["resources"].([]string)
	if !ok || len(resources) != 2 || resources[0] != "scripts/run.sh" {
		t.Fatalf("resources = %v", result["resources"])
	}
}

func TestLoadSkillToolUnknownNameListsAvailable(t *testing.T) {
	tool := &LoadSkillTool{Skills: []*skills.Skill{
		testSkill("alpha", "First skill", "body"),
		testSkill("zeta", "Second skill", "body"),
	}}
	_, err := tool.Call(context.Background(), map[string]any{"name": "missing"})
	if err == nil {
		t.Fatalf("expected error for unknown skill")
	}
	if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "zeta") {
		t.Fatalf("error must list available skills: %v", err)
	}
}

func TestLoadSkillToolNoSkills(t *testing.T) {
	tool := &LoadSkillTool{}
	_, err := tool.Call(context.Background(), map[string]any{"name": "anything"})
	if err == nil || !strings.Contains(err.Error(), "no skills") {
		t.Fatalf("err = %v, want no-skills error", err)
	}
}

func TestLoadSkillToolMissingNameArg(t *testing.T) {
	tool := &LoadSkillTool{Skills: []*skills.Skill{testSkill("alpha", "d", "body")}}
	_, err := tool.Call(context.Background(), map[string]any{})
	if err == nil {
		t.Fatalf("expected error for missing name argument")
	}
}

// Ensure the provider is a valid ContextProvider and the tool a valid Tool.
var (
	_ ContextProvider = (*SkillContextProvider)(nil)
	_ tools.Tool      = (*LoadSkillTool)(nil)
)
