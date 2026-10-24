package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"enterprise-agent-platform/packages/skillreg"
)

// skillFiles turns a developer checkout into the exact standard package
// entries the server receives. It rejects symlinks and delegates path/content
// validation and secret scanning to the same deterministic packager used by
// the registry.
func skillFiles(dir string) ([]skillreg.PackageFile, error) {
	var files []skillreg.PackageFile
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not allowed in skills: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, skillreg.PackageFile{Path: filepath.ToSlash(rel), Content: string(content)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !hasSkillFile(files, "SKILL.md") || !hasSkillFile(files, "skill.yaml") {
		return nil, fmt.Errorf("skill requires SKILL.md and skill.yaml")
	}
	_, _, err = skillreg.BuildArchive(files)
	if err != nil {
		return nil, err
	}
	return files, nil
}

func hasSkillFile(files []skillreg.PackageFile, name string) bool {
	for _, f := range files {
		if strings.EqualFold(f.Path, name) {
			return true
		}
	}
	return false
}
