// Package skills loads SKILL.md-format skills for agent use.
//
// A skill is a directory containing a SKILL.md file with a minimal
// frontmatter block (the package intentionally does not depend on a YAML
// library):
//
//	---
//	name: my-skill
//	description: What the skill does
//	resources:
//	  - scripts/run.sh
//	  - templates/prompt.md
//	---
//
//	# Body
//
//	The body is returned verbatim by LoadSkillTool; resources are declared
//	as relative paths and are never read by this package or the agent.
package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Sentinel errors returned by the loaders.
var (
	ErrInvalidName        = errors.New("skills: invalid skill name")
	ErrMissingDescription = errors.New("skills: skill description is required")
	ErrTooLarge           = errors.New("skills: SKILL.md exceeds size limit")
	ErrInvalidResource    = errors.New("skills: invalid resource path")
)

// maxSkillBytes is the maximum accepted SKILL.md size (1 MiB).
const maxSkillBytes = 1 << 20

// Skill is one loaded SKILL.md.
type Skill struct {
	// Name is the validated skill identifier.
	Name string
	// Description is the required one-line summary.
	Description string
	// Body is the SKILL.md content after the frontmatter block.
	Body string
	// Resources are the declared relative paths (never read here).
	Resources []string
	// Dir is the absolute directory that contains the SKILL.md.
	Dir string
}

var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load reads and parses a single SKILL.md file.
func Load(path string) (*Skill, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSkillBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, info.Size())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	skill, err := parse(raw, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("skills: parse %s: %w", path, err)
	}
	return skill, nil
}

// LoadDirs loads one SKILL.md from each directory in dirs. A directory
// without a SKILL.md is an error; use Discover to scan a tree instead.
func LoadDirs(dirs []string) ([]*Skill, error) {
	var skills []*Skill
	for _, dir := range dirs {
		if dir == "" {
			return nil, errors.New("skills: empty skill directory")
		}
		skill, err := Load(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			return nil, fmt.Errorf("skills: load dir %q: %w", dir, err)
		}
		skills = append(skills, skill)
	}
	return skills, nil
}

// Discover walks roots recursively and loads every directory that contains
// a SKILL.md. Results are sorted by name for deterministic ordering.
func Discover(roots []string) ([]*Skill, error) {
	var skills []*Skill
	seen := make(map[string]struct{})
	for _, root := range roots {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || d.Name() != "SKILL.md" {
				return nil
			}
			skill, loadErr := Load(path)
			if loadErr != nil {
				return loadErr
			}
			if _, dup := seen[skill.Name]; dup {
				return fmt.Errorf("skills: duplicate skill name %q in %s", skill.Name, root)
			}
			seen[skill.Name] = struct{}{}
			skills = append(skills, skill)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("skills: discover root %q: %w", root, err)
		}
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

// parse extracts frontmatter and body from a SKILL.md byte slice. The
// frontmatter is a ----delimited block of single-line "key: value" fields
// plus an optional "resources:" list of "- item" lines.
func parse(raw []byte, dir string) (*Skill, error) {
	text := string(raw)
	skill := &Skill{Dir: dir}

	// Optional frontmatter: starts with a --- line.
	if strings.HasPrefix(text, "---\n") {
		rest := text[len("---\n"):]
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return nil, errors.New("unterminated frontmatter block")
		}
		if err := parseFrontmatter(rest[:end], skill); err != nil {
			return nil, err
		}
		text = rest[end+len("\n---"):]
		text = strings.TrimPrefix(text, "\n")
	}

	if !skillNamePattern.MatchString(skill.Name) {
		return nil, fmt.Errorf("%w: %q (must match ^[a-z0-9][a-z0-9-]*$)", ErrInvalidName, skill.Name)
	}
	if skill.Description == "" {
		return nil, ErrMissingDescription
	}
	skill.Body = strings.TrimSpace(text)
	return skill, nil
}

func parseFrontmatter(block string, skill *Skill) error {
	lines := strings.Split(block, "\n")
	inResources := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if inResources {
			if strings.HasPrefix(trimmed, "-") {
				resource := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
				if err := validateResource(resource); err != nil {
					return err
				}
				skill.Resources = append(skill.Resources, resource)
				continue
			}
			inResources = false
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			// Ignore unsupported frontmatter lines rather than failing:
			// skills may carry other metadata.
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "name":
			skill.Name = value
		case "description":
			skill.Description = value
		case "resources":
			inResources = true
		}
	}
	return nil
}

// validateResource rejects absolute paths, empty entries, and any path
// containing a ".." segment.
func validateResource(resource string) error {
	if resource == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidResource)
	}
	if filepath.IsAbs(resource) {
		return fmt.Errorf("%w: %q must be relative", ErrInvalidResource, resource)
	}
	for _, segment := range strings.Split(resource, "/") {
		if segment == ".." {
			return fmt.Errorf("%w: %q must not contain \"..\"", ErrInvalidResource, resource)
		}
	}
	return nil
}
