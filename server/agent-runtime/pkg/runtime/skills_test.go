package runtime_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	agentruntime "github.com/linxinhong/xingding-agent-runtime/pkg/runtime"
)

const skillMD = `---
name: demo-skill
description: Demonstrates a skill
resources:
  - scripts/run.sh
---

# Demo

Skill body content.
`

func writeSkillDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

func testDeps() agentruntime.Dependencies {
	return agentruntime.Dependencies{Models: map[string]models.Model{"chat": mock.New()}}
}

// v1-v3 specs with skill_dirs/skill_roots must be rejected at decode time.
func TestDecodeSpecRejectsSkillFieldsBelowV4(t *testing.T) {
	for _, v := range []int{1, 2, 3} {
		input := `{"schema_version":` + strconv.Itoa(v) + `,"id":"v","version":"v1","agents":[{"id":"a","model_ref":"m","skill_dirs":["skills/demo"]}]}`
		_, err := agentruntime.DecodeSpec(strings.NewReader(input))
		var buildErr *agentruntime.BuildError
		if !errors.As(err, &buildErr) || len(buildErr.Diagnostics) != 1 {
			t.Fatalf("v%d: expected 1 diagnostic, got %v", v, err)
		}
		if buildErr.Diagnostics[0].Code != agentruntime.DiagnosticInvalidValue ||
			!strings.Contains(buildErr.Diagnostics[0].Message, "skill_dirs requires schema_version 4") {
			t.Fatalf("v%d: unexpected diagnostic %+v", v, buildErr.Diagnostics[0])
		}
	}
}

// v4 spec with skill fields decodes cleanly.
func TestDecodeSpecV4AcceptsSkillFields(t *testing.T) {
	input := `{"schema_version":4,"id":"v4","version":"v1","agents":[{"id":"a","model_ref":"m","skill_dirs":["skills/demo"],"skill_roots":["skills"]}]}`
	decoded, err := agentruntime.DecodeSpec(strings.NewReader(input))
	if err != nil {
		t.Fatalf("DecodeSpec v4: %v", err)
	}
	if len(decoded.Agents[0].SkillDirs) != 1 || decoded.Agents[0].SkillDirs[0] != "skills/demo" {
		t.Fatalf("skill_dirs = %+v", decoded.Agents[0].SkillDirs)
	}
	if len(decoded.Agents[0].SkillRoots) != 1 || decoded.Agents[0].SkillRoots[0] != "skills" {
		t.Fatalf("skill_roots = %+v", decoded.Agents[0].SkillRoots)
	}
}

// Build with a v3 spec carrying skill_dirs must fail with the v4 diagnostic.
func TestBuildRejectsSkillFieldsBelowV4(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		SchemaVersion: 3, ID: "p", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "a", ModelRef: "chat", SkillDirs: []string{"skills/demo"}}},
	}
	_, err := agentruntime.Build(spec, testDeps())
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("expected BuildError, got %v", err)
	}
	found := false
	for _, d := range buildErr.Diagnostics {
		if strings.Contains(d.Message, "skill_dirs requires schema_version 4") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected skill_dirs v4 diagnostic, got %+v", buildErr.Diagnostics)
	}
}

// Build with skill_dirs assembles the agent successfully.
func TestBuildWithSkillDirsSucceeds(t *testing.T) {
	root := t.TempDir()
	writeSkillDir(t, filepath.Join(root, "demo"))
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion, ID: "p", Version: "v1",
		Agents: []agentruntime.AgentSpec{{
			ID: "a", ModelRef: "chat", SkillDirs: []string{filepath.Join(root, "demo")},
		}},
	}
	project, err := agentruntime.Build(spec, testDeps())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if project == nil {
		t.Fatalf("project is nil")
	}
	if _, ok := project.Agent("a"); !ok {
		t.Fatalf("agent not built")
	}
}

// Build with a broken skill directory fails fast.
func TestBuildFailsFastOnBrokenSkill(t *testing.T) {
	root := t.TempDir() // empty: no SKILL.md
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion, ID: "p", Version: "v1",
		Agents: []agentruntime.AgentSpec{{
			ID: "a", ModelRef: "chat", SkillDirs: []string{filepath.Join(root, "missing")},
		}},
	}
	_, err := agentruntime.Build(spec, testDeps())
	if err == nil {
		t.Fatalf("expected fail-fast on broken skill dir")
	}
}
