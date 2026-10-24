// SkillAdapter is the local installation SPI (TASK-018, ADR-0017): the
// server ships standard packages, while each adapter owns the concrete
// filesystem convention of one supported Agent client. Adapters never run
// package code and never read or write anything outside their root.
package cli

import (
	"os"
	"path/filepath"
)

// SkillAdapter maps a client name to its managed skill root.
type SkillAdapter interface {
	Name() string
	SkillRoot(workdir string) (string, error)
}

type homeAdapter struct{ name, subpath string }

func (a homeAdapter) Name() string { return a.name }

func (a homeAdapter) SkillRoot(string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, a.subpath), nil
}

type projectAdapter struct{}

func (projectAdapter) Name() string { return "project" }

func (a projectAdapter) SkillRoot(workdir string) (string, error) {
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		workdir = wd
	}
	return filepath.Join(workdir, ".agents", "skills"), nil
}

// skillAdapters is the supported qualification set: Harness and Codex are the
// mandatory pair; claude-code, pi and the project-local layout follow the
// standard .agents/skills convention.
var skillAdapters = map[string]SkillAdapter{
	"harness":     homeAdapter{name: "harness", subpath: filepath.Join(".agents", "skills")},
	"codex":       homeAdapter{name: "codex", subpath: filepath.Join(".agents", "skills")},
	"claude-code": homeAdapter{name: "claude-code", subpath: filepath.Join(".claude", "skills")},
	"pi":          homeAdapter{name: "pi", subpath: filepath.Join(".pi", "agent", "skills")},
	"project":     projectAdapter{},
}

// lookupSkillAdapter resolves an adapter by client name.
func lookupSkillAdapter(client string) (SkillAdapter, bool) {
	a, ok := skillAdapters[client]
	return a, ok
}

// AgentSkillRoot resolves the managed skill root for a client via the
// adapter SPI, falling back to the project layout for unknown names.
func AgentSkillRoot(workdir, client string) string {
	if a, ok := lookupSkillAdapter(client); ok {
		root, err := a.SkillRoot(workdir)
		if err == nil {
			return root
		}
	}
	root, _ := projectAdapter{}.SkillRoot(workdir)
	return root
}
