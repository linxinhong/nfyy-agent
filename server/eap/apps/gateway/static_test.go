package main_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gateway "enterprise-agent-platform/apps/gateway"
)

// fixtureDist builds a minimal Vite-shaped dist:
// index.html + content-hashed asset + favicon at the root.
func fixtureDist(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("index.html", "<!doctype html><html><body>portal shell</body></html>")
	write("assets/app-a1b2c3.js", "console.log(1)")
	write("favicon.svg", "<svg/>")
	return dir
}

func newSiteHandler(t *testing.T, dist string) http.Handler {
	t.Helper()
	site, err := gateway.NewStaticSite(dist)
	if err != nil {
		t.Fatalf("new static site: %v", err)
	}
	return site.Handler()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStaticServesIndexAndAssets(t *testing.T) {
	h := newSiteHandler(t, fixtureDist(t))

	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("/: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "portal shell") {
		t.Fatalf("/ must serve the built index.html: %s", rec.Body.String())
	}

	rec = get(t, h, "/assets/app-a1b2c3.js")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log(1)") {
		t.Fatalf("asset: %d %s", rec.Code, rec.Body.String())
	}
}

// SPA fallback: deep portal routes render the shell so refresh works.
func TestStaticSPAFallback(t *testing.T) {
	h := newSiteHandler(t, fixtureDist(t))
	for _, path := range []string{"/portal/keys", "/portal/agent-center", "/login"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "portal shell") {
			t.Errorf("SPA fallback %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

// Security regression: unknown API paths get JSON problems, never the HTML
// shell — machines must not parse a portal page as an API response.
func TestUnknownAPIPathsNeverReturnHTML(t *testing.T) {
	h := newSiteHandler(t, fixtureDist(t))
	for _, path := range []string{
		"/v1/models", "/v1/unknown", "/enterprise/v1/bootstrap",
		"/enterprise/v1/knowledge/search", "/mcp/unknown", "/.well-known/unknown",
	} {
		rec := get(t, h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "portal shell") || strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("%s: API 404 must not return HTML: %s", path, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s: content type %q, want JSON", path, ct)
		}
	}
}

// Security and cache headers are part of the deployment contract.
func TestStaticSecurityAndCacheHeaders(t *testing.T) {
	h := newSiteHandler(t, fixtureDist(t))

	rec := get(t, h, "/portal/keys")
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("nosniff missing")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("frame-options missing")
	}
	if rec.Header().Get("Referrer-Policy") == "" {
		t.Errorf("referrer-policy missing")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index/SPA cache-control %q, want no-cache", cc)
	}

	rec = get(t, h, "/assets/app-a1b2c3.js")
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("hashed assets must be immutable, got %q", cc)
	}
}

// Missing dist is a configuration error at startup, not a runtime panic.
func TestNewStaticSiteRejectsMissingDist(t *testing.T) {
	if _, err := gateway.NewStaticSite(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatalf("missing dist must be rejected")
	}
}
