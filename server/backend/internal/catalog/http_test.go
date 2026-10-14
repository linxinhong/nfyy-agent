package catalog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"yuanpu.dev/server/internal/httpkit"
)

func catalogHandler(t *testing.T, root string) http.Handler {
	t.Helper()
	module, err := NewHTTP(root)
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(httpkit.RequestMetadata)
	module.Mount(router)
	return router
}

func request(t *testing.T, handler http.Handler, path string, status int) *httptest.ResponseRecorder {
	t.Helper()
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, path, nil))
	if result.Code != status {
		t.Fatalf("%s: status %d, want %d; body %s", path, result.Code, status, result.Body.String())
	}
	return result
}

func TestCatalogSearchAndDetailRemainCompatible(t *testing.T) {
	handler := catalogHandler(t, "")
	var found struct {
		SchemaVersion int    `json:"schemaVersion"`
		Items         []Item `json:"items"`
	}
	if err := json.Unmarshal(request(t, handler, "/v1/catalog/search?q=Python", 200).Body.Bytes(), &found); err != nil {
		t.Fatal(err)
	}
	if found.SchemaVersion != 1 || len(found.Items) != 1 || found.Items[0].ID != "builtin.python.echo" {
		t.Fatalf("unexpected search response: %+v", found)
	}
	if found.Items[0].Source != "artifact:/v1/capability-packages/builtin.python.echo/manifest" {
		t.Fatalf("artifact source changed: %s", found.Items[0].Source)
	}
	var detail Item
	if err := json.Unmarshal(request(t, handler, "/v1/catalog/items/builtin.python.echo", 200).Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Components) != 1 || detail.Components[0] != "connector" {
		t.Fatalf("unexpected detail: %+v", detail)
	}
	request(t, handler, "/v1/catalog/search?q=pi-mcp-adapter", 200)
	request(t, handler, "/v1/catalog/items/not-found", 404)
	request(t, handler, "/v1/capability-packages/builtin.python.echo/manifest", 404)
}

func TestCatalogServesOnlyManifestReferencedRegularArchives(t *testing.T) {
	root := t.TempDir()
	archive := "YuanpuEchoMcp-linux-x64.tar.gz"
	manifest := `{"id":"builtin.python.echo","version":"9.8.7","artifacts":[{"url":"artifacts/` + archive + `"},{"url":"artifacts/private.pem"}],"signature":{"value":"fixture"}}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, archive), []byte("signed-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "private.pem"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	handler := catalogHandler(t, root)
	var found struct {
		Items []Item `json:"items"`
	}
	if err := json.Unmarshal(request(t, handler, "/v1/catalog/search?q=Python", 200).Body.Bytes(), &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Items) != 1 || found.Items[0].Version != "9.8.7" {
		t.Fatalf("published version missing: %+v", found)
	}
	if got := request(t, handler, "/v1/capability-packages/builtin.python.echo/manifest", 200).Body.String(); got != manifest+"\n" {
		t.Fatalf("manifest changed: %q", got)
	}
	artifact := request(t, handler, "/v1/capability-packages/builtin.python.echo/artifacts/"+archive, 200)
	if artifact.Body.String() != "signed-fixture" || artifact.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("unexpected artifact: body=%q headers=%v", artifact.Body.String(), artifact.Header())
	}
	request(t, handler, "/v1/capability-packages/builtin.python.echo/artifacts/private.pem", 404)
	request(t, handler, "/v1/capability-packages/builtin.python.echo/artifacts/..%2Fsecret", 400)
	link := "YuanpuEchoMcp-darwin-arm64.tar.gz"
	if err := os.Symlink(filepath.Join(root, "private.pem"), filepath.Join(root, link)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	linkedManifest := `{"id":"builtin.python.echo","version":"9.8.7","artifacts":[{"url":"artifacts/` + link + `"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(linkedManifest), 0600); err != nil {
		t.Fatal(err)
	}
	request(t, handler, "/v1/capability-packages/builtin.python.echo/artifacts/"+link, 404)
}
