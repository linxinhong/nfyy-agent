package capability

import (
	"context"
	"strings"
)

// SkillEntry is one released, workspace-scoped skill version as capability
// metadata. The gateway supplies this from skill_packages control data
// (released status only).
type SkillEntry struct {
	Name        string
	Version     string
	Description string
}

// AdaptSkills lifts minimal store rows into entries (description optional).
func AdaptSkills(minimal []struct{ Name, Version string }) []SkillEntry {
	out := make([]SkillEntry, 0, len(minimal))
	for _, m := range minimal {
		out = append(out, SkillEntry{Name: m.Name, Version: m.Version})
	}
	return out
}

// SkillSource adapts released skill packages into capabilities. Skill
// BODIES are never executed here: execute returns governed, verified
// install guidance (ADR-0017 — the platform ships packages, local eap sync
// runs them).
type SkillSource struct {
	// ListSkills returns released entries visible to the workspace.
	ListSkills func(ctx context.Context, workspaceID string) ([]SkillEntry, error)
	// ResolveSkill verifies the exact name[@version] is released and visible.
	ResolveSkill func(ctx context.Context, workspaceID, name, version string) (SkillEntry, error)
	// DownloadTicket mints a same-domain download ticket when the source can;
	// optional (guidance falls back to the sync command).
	DownloadTicket func(ctx context.Context, workspaceID, name, version string) (ticket string, err error)
}

// Kind implements Source.
func (s *SkillSource) Kind() string { return string(KindSkill) }

func (s *SkillSource) Search(ctx context.Context, sub Subject, q SearchQuery) ([]Capability, error) {
	if s.ListSkills == nil {
		return nil, nil
	}
	skills, err := s.ListSkills(ctx, sub.WorkspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]Capability, 0, len(skills))
	for _, sk := range skills {
		// latest released version only in discovery; @version pins at execute
		out = append(out, Capability{
			Name:        "skill:" + sk.Name,
			Description: sk.Description + " (v" + sk.Version + ", released)",
			Type:        "skill",
			RiskLevel:   "R0", // platform never executes skill bodies (ADR-0017)
			Status:      StatusAvailable,
			InputHint:   "no arguments: skills install locally via `eap skill sync`",
		})
	}
	return out, nil
}

func (s *SkillSource) Parse(ctx context.Context, sub Subject, name string) (*Parsed, error) {
	if !strings.HasPrefix(name, "skill:") {
		return nil, nil
	}
	rest := strings.TrimPrefix(name, "skill:")
	skillName, version := splitNameVersion(rest)
	if s.ResolveSkill == nil {
		return nil, nil
	}
	if _, err := s.ResolveSkill(ctx, sub.WorkspaceID, skillName, version); err != nil {
		return nil, nil // invisible or unknown: next source may own it
	}
	return &Parsed{Source: KindSkill, Name: name, Risk: "R0"}, nil
}

func (s *SkillSource) Execute(ctx context.Context, sub Subject, parsed *Parsed, in ExecuteInput) (ExecuteResult, error) {
	rest := strings.TrimPrefix(parsed.Name, "skill:")
	skillName, version := splitNameVersion(rest)
	if version == "" {
		if s.ResolveSkill == nil {
			return ExecuteResult{}, ErrUnknown(parsed.Name)
		}
		entry, err := s.ResolveSkill(ctx, sub.WorkspaceID, skillName, "")
		if err != nil {
			return ExecuteResult{}, ErrUnknown(parsed.Name)
		}
		version = entry.Version
	}
	var b strings.Builder
	b.WriteString("capability skill:" + skillName + "@" + version + " is released and verified.\n")
	b.WriteString("Skills run locally, never on the platform (ADR-0017).\n")
	b.WriteString("Install into your Agent clients:\n  eap skill install " + skillName + "@" + version + "\n")
	b.WriteString("Or declare it in .eap/workspace.yaml and run `eap sync`.\n")
	if s.DownloadTicket != nil {
		if ticket, err := s.DownloadTicket(ctx, sub.WorkspaceID, skillName, version); err == nil && ticket != "" {
			b.WriteString("Direct download ticket (single use, short lived): " + ticket + "\n")
		}
	}
	return ExecuteResult{Content: b.String(), RiskLevel: "R0", Kind: "skill"}, nil
}

// splitNameVersion parses "name" or "name@version".
func splitNameVersion(arg string) (string, string) {
	if i := strings.LastIndex(arg, "@"); i > 0 {
		return arg[:i], arg[i+1:]
	}
	return arg, ""
}
