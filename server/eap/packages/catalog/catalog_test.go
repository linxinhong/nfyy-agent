package catalog

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSearchAndItemWithoutArtifacts(t *testing.T) {
	d, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	found := d.Search("Python", nil)
	if len(found) != 1 || found[0].ID != PythonEchoID {
		t.Fatalf("search: %+v", found)
	}
	if found[0].Source != "artifact:/enterprise/v1/capability-packages/builtin.python.echo/manifest" {
		t.Fatalf("artifact source: %s", found[0].Source)
	}
	if _, err := d.Item("not-found", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item: %v", err)
	}
	if _, err := d.Manifest(PythonEchoID); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("manifest without root: %v", err)
	}
	extra := []Item{RegistryItem("incident-guide", "1.2.0", "secops")}
	if got := d.Search("incident", extra); len(got) != 1 || got[0].Source != "registry:incident-guide@1.2.0" {
		t.Fatalf("registry merge: %+v", got)
	}
	if got, err := d.Item("registry.incident-guide", extra); err != nil || got.Version != "1.2.0" {
		t.Fatalf("registry item: %+v %v", got, err)
	}
}

func TestOnlyManifestReferencedRegularArchivesAreServed(t *testing.T) {
	root := t.TempDir()
	archive := "YuanpuEchoMcp-linux-x64.tar.gz"
	manifest := `{"id":"builtin.python.echo","version":"9.8.7","artifacts":[{"url":"artifacts/` + archive + `"},{"url":"artifacts/private.pem"}]}`
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", manifest)
	write(archive, "signed-fixture")
	write("private.pem", "secret")
	d, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Search("Python", nil); got[0].Version != "9.8.7" {
		t.Fatalf("published version: %+v", got)
	}
	if raw, err := d.Manifest(PythonEchoID); err != nil || string(raw) != manifest {
		t.Fatalf("manifest: %q %v", raw, err)
	}
	f, err := d.OpenArtifact(PythonEchoID, archive)
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	body, _ := io.ReadAll(f)
	f.Close()
	if string(body) != "signed-fixture" {
		t.Fatalf("artifact body: %q", body)
	}
	for name, want := range map[string]error{
		"private.pem": ErrNotPublished,
		"../secret":   ErrInvalidPath,
		"a/b.tar.gz":  ErrInvalidPath,
		"":            ErrInvalidPath,
		archive + "x": ErrNotPublished,
	} {
		if _, err := d.OpenArtifact(PythonEchoID, name); !errors.Is(err, want) {
			t.Errorf("%q: got %v want %v", name, err, want)
		}
	}
	if _, err := d.OpenArtifact("other.package", archive); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("other package: %v", err)
	}
	link := "YuanpuEchoMcp-darwin-arm64.tar.gz"
	if err := os.Symlink(filepath.Join(root, "private.pem"), filepath.Join(root, link)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	write("manifest.json", `{"id":"builtin.python.echo","version":"9.8.7","artifacts":[{"url":"artifacts/`+link+`"}]}`)
	if _, err := d.OpenArtifact(PythonEchoID, link); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("symlinked archive must not be served: %v", err)
	}
}
