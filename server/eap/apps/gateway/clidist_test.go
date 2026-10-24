// CLI installer distribution (TASK-024): origin injection, allowlisted
// platform serving with digest headers, checksums list, and refusal of
// unknown platforms / path traversal / disabled dist.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIDistInstaller(t *testing.T) {
	dist := t.TempDir()
	bin := []byte("fake-eap-binary-bytes")
	if err := os.WriteFile(filepath.Join(dist, "eap-linux-amd64"), bin, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := &Server{CLIDist: dist}
	handler := srv.Handler()

	// 1. Install script carries the request origin, no template placeholder.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gw.example.com/cli/install.sh", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("install.sh: %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "{{ORIGIN}}") || !strings.Contains(body, `ORIGIN="http://gw.example.com"`) {
		t.Fatalf("origin not injected: %.200s", body)
	}
	if !strings.Contains(body, "sha256") {
		t.Fatalf("installer must verify checksums")
	}

	// 2. Binary serving for an allowlisted platform, with digest header.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gw.example.com/cli/install/linux-amd64", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != string(bin) {
		t.Fatalf("binary: %d %.60s", rec.Code, rec.Body.String())
	}
	want := sha256.Sum256(bin)
	if rec.Header().Get("X-EAP-Sha256") != hex.EncodeToString(want[:]) {
		t.Fatalf("digest header missing or wrong")
	}

	// 3. Unknown platform and traversal attempts fail closed.
	for _, platform := range []string{"unknown-arch", "darwin", "%2e%2e"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gw.example.com/cli/install/"+platform, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("platform %q must 404, got %d", platform, rec.Code)
		}
	}
	// Path-traversal shapes are refused outright: cleaned-path redirects or
	// 404s, never a binary.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gw.example.com/cli/install/..", nil))
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("traversal must not serve bytes: %d", rec.Code)
	}

	// 4. Checksums list serves the dist inventory.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gw.example.com/cli/install/sha256sums", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "eap-linux-amd64") {
		t.Fatalf("checksums: %d %s", rec.Code, rec.Body.String())
	}

	// 5. Disabled dist: routes not registered → 404 from the mux.
	disabled := (&Server{}).Handler()
	rec = httptest.NewRecorder()
	disabled.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gw.example.com/cli/install.sh", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled dist must 404: %d", rec.Code)
	}
}
