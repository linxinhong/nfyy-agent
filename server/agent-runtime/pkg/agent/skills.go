package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/skills"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// SkillContextProvider injects a catalog of available skills (name plus
// description only) into the system segment. It implements progressive
// disclosure: the model learns what skills exist and calls load_skill to
// fetch a body on demand. An empty skill list is a pass-through.
type SkillContextProvider struct {
	// Skills are the loaded skills available to the agent.
	Skills []*skills.Skill
}

// Apply implements ContextProvider.
func (p *SkillContextProvider) Apply(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	if len(p.Skills) == 0 {
		return req, nil
	}
	var b strings.Builder
	b.WriteString("Available skills — call load_skill with a skill name to read its full content:\n")
	for i, s := range p.Skills {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- %s: %s", s.Name, s.Description)
	}
	req.System = append(req.System, models.Message{
		Role:    models.RoleSystem,
		Content: b.String(),
	})
	return req, nil
}

// Compile-time checks.
var (
	_ ContextProvider     = (*SkillContextProvider)(nil)
	_ tools.Tool          = (*LoadSkillTool)(nil)
	_ tools.ApprovalAware = (*LoadSkillTool)(nil)
)

// LoadSkillTool returns a skill's body and declared resource paths by name.
// It never reads resource files on the agent's behalf; the body is plain
// text destined for the system segment. Unknown names return an error that
// lists the available skills.
type LoadSkillTool struct {
	// Skills are the loaded skills the tool can return.
	Skills []*skills.Skill
}

// Name implements tools.Tool.
func (t *LoadSkillTool) Name() string { return "load_skill" }

// Description implements tools.Tool.
func (t *LoadSkillTool) Description() string {
	return "Load the full content of a skill by name. Returns the skill body and its declared resource paths."
}

// Schema implements tools.Tool.
func (t *LoadSkillTool) Schema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name(),
			"description": t.Description(),
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "Skill name to load"},
				},
				"required": []string{"name"},
			},
		},
	}
}

// Approval implements tools.ApprovalAware: loading a skill body is a read
// and never requires approval.
func (t *LoadSkillTool) Approval() tools.ApprovalKind { return tools.ApprovalNone }

// Call implements tools.Tool.
func (t *LoadSkillTool) Call(ctx context.Context, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	for _, s := range t.Skills {
		if s.Name == name {
			return map[string]any{
				"name":        s.Name,
				"description": s.Description,
				"body":        s.Body,
				"resources":   s.Resources,
			}, nil
		}
	}
	available := make([]string, 0, len(t.Skills))
	for _, s := range t.Skills {
		available = append(available, s.Name)
	}
	sort.Strings(available)
	if len(available) == 0 {
		return nil, fmt.Errorf("load_skill: no skills are available")
	}
	return nil, fmt.Errorf("load_skill: skill %q not found; available: %s", name, strings.Join(available, ", "))
}
