package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill writes a SKILL.md under dir and returns its path.
func writeSkill(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

const validSkill = `---
name: my-skill
description: Does something useful
resources:
  - scripts/run.sh
  - templates/prompt.md
---

# Body

Do the thing.
`

func TestLoadParsesFrontmatterAndBody(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, validSkill)

	skill, err := Load(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if skill.Name != "my-skill" {
		t.Fatalf("name = %q, want my-skill", skill.Name)
	}
	if skill.Description != "Does something useful" {
		t.Fatalf("description = %q", skill.Description)
	}
	if !strings.Contains(skill.Body, "Do the thing.") {
		t.Fatalf("body missing content: %q", skill.Body)
	}
	if len(skill.Resources) != 2 || skill.Resources[0] != "scripts/run.sh" || skill.Resources[1] != "templates/prompt.md" {
		t.Fatalf("resources = %v", skill.Resources)
	}
	if skill.Dir != dir {
		t.Fatalf("dir = %q, want %q", skill.Dir, dir)
	}
}

func TestLoadWithoutFrontmatterFails(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "# Body only\n")
	if _, err := Load(filepath.Join(dir, "SKILL.md")); err == nil {
		t.Fatalf("expected error for missing name/description")
	}
}

func TestLoadInvalidName(t *testing.T) {
	for _, name := range []string{"", "My-Skill", "my_skill", "my skill", "-leading", "a..b"} {
		dir := t.TempDir()
		content := "---\nname: " + name + "\ndescription: d\n---\nbody\n"
		writeSkill(t, dir, content)
		_, err := Load(filepath.Join(dir, "SKILL.md"))
		if err == nil || !strings.Contains(err.Error(), "invalid skill name") {
			t.Fatalf("name %q: err = %v, want invalid name", name, err)
		}
	}
}

func TestLoadValidNames(t *testing.T) {
	for _, name := range []string{"a", "skill", "my-skill", "1abc", "a1b2"} {
		dir := t.TempDir()
		content := "---\nname: " + name + "\ndescription: d\n---\nbody\n"
		writeSkill(t, dir, content)
		if _, err := Load(filepath.Join(dir, "SKILL.md")); err != nil {
			t.Fatalf("name %q: err = %v", name, err)
		}
	}
}

func TestLoadMissingDescription(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "---\nname: ok-name\n---\nbody\n")
	_, err := Load(filepath.Join(dir, "SKILL.md"))
	if err == nil || !strings.Contains(err.Error(), "description is required") {
		t.Fatalf("err = %v, want missing description", err)
	}
}

func TestLoadTooLarge(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxSkillBytes+1)
	writeSkill(t, dir, big)
	_, err := Load(filepath.Join(dir, "SKILL.md"))
	if err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("err = %v, want size limit", err)
	}
}

func TestLoadRejectsInvalidResources(t *testing.T) {
	for _, resource := range []string{"", "/abs/path", "../escape", "a/../b", ".."} {
		dir := t.TempDir()
		content := "---\nname: r-skill\ndescription: d\nresources:\n  - " + resource + "\n---\nbody\n"
		writeSkill(t, dir, content)
		_, err := Load(filepath.Join(dir, "SKILL.md"))
		if err == nil || !strings.Contains(err.Error(), "invalid resource path") {
			t.Fatalf("resource %q: err = %v, want invalid resource", resource, err)
		}
	}
}

func TestLoadIgnoresUnknownFrontmatterFields(t *testing.T) {
	dir := t.TempDir()
	content := `---
name: extra-skill
description: d
author: someone
tags: a, b
---
body
`
	writeSkill(t, dir, content)
	skill, err := Load(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if skill.Name != "extra-skill" || skill.Description != "d" {
		t.Fatalf("skill = %+v", skill)
	}
}

func TestLoadDirs(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "one"), validSkill)
	writeSkill(t, filepath.Join(root, "two"), strings.Replace(validSkill, "my-skill", "second-skill", 1))

	skills, err := LoadDirs([]string{filepath.Join(root, "one"), filepath.Join(root, "two")})
	if err != nil {
		t.Fatalf("LoadDirs: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("loaded %d skills, want 2", len(skills))
	}
}

func TestLoadDirsMissingSkillFails(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadDirs([]string{root}); err == nil {
		t.Fatalf("expected error for dir without SKILL.md")
	}
}

func TestDiscoverFindsNestedSkillsSorted(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "z-skill"), strings.Replace(validSkill, "my-skill", "z-skill", 1))
	writeSkill(t, filepath.Join(root, "nested", "a-skill"), strings.Replace(validSkill, "my-skill", "a-skill", 1))

	skills, err := Discover([]string{root})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("found %d skills, want 2", len(skills))
	}
	if skills[0].Name != "a-skill" || skills[1].Name != "z-skill" {
		t.Fatalf("not sorted: %s, %s", skills[0].Name, skills[1].Name)
	}
}

func TestDiscoverDuplicateNameFails(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "one"), validSkill)
	writeSkill(t, filepath.Join(root, "two"), validSkill) // same name
	if _, err := Discover([]string{root}); err == nil || !strings.Contains(err.Error(), "duplicate skill name") {
		t.Fatalf("err = %v, want duplicate name", err)
	}
}
