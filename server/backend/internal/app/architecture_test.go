package app

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The HTTP host is business-neutral; domain adapters may use the shared
// transport kit, but they must not import the host or composition root.
func TestModuleImportBoundaries(t *testing.T) {
	for _, tc := range []struct {
		dir       string
		forbidden []string
	}{
		{"../httpapi", []string{"/internal/identity", "/internal/content", "/internal/app"}},
		{"../identity", []string{"/internal/httpapi", "/internal/content", "/internal/app"}},
		{"../content", []string{"/internal/httpapi", "/internal/app"}},
	} {
		packages, err := parser.ParseDir(token.NewFileSet(), tc.dir, func(info fs.FileInfo) bool {
			return strings.HasSuffix(info.Name(), ".go") && !strings.HasSuffix(info.Name(), "_test.go")
		}, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, pkg := range packages {
			for filename, file := range pkg.Files {
				for _, imported := range file.Imports {
					path, err := strconv.Unquote(imported.Path.Value)
					if err != nil {
						t.Fatal(err)
					}
					for _, suffix := range tc.forbidden {
						if strings.HasSuffix(path, suffix) {
							t.Errorf("%s imports forbidden owner %s", filepath.Base(filename), path)
						}
					}
				}
			}
		}
	}
}
