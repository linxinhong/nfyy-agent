package main

import (
	"io/fs"
	"net/http"
	"os"
	"strings"

	"enterprise-agent-platform/packages/errs"
)

// apiPrefixes are gateway-owned namespaces: a request under them is an API
// call and must NEVER fall back to index.html. Unknown paths inside them get
// a JSON problem (fail closed for machines), not an HTML shell.
var apiPrefixes = []string{"/enterprise", "/v1", "/mcp", "/.well-known", "/debug"}

func isAPIPath(path string) bool {
	if path == "/healthz" || path == "/readyz" {
		return true
	}
	for _, prefix := range apiPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// StaticSite serves the built Portal SPA from disk with SPA fallback.
type StaticSite struct {
	dist      fs.FS
	indexHTML []byte
}

// NewStaticSite validates and preloads the dist's index.html.
func NewStaticSite(distDir string) (*StaticSite, error) {
	dist := os.DirFS(distDir)
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "web dist is missing index.html")
	}
	return &StaticSite{dist: dist, indexHTML: index}, nil
}

// Handler returns the catch-all static handler. Route precedence is handled
// by ServeMux: registered API patterns win; everything that reaches here is
// either a static asset, a portal route (SPA fallback), or an unknown API
// path (JSON 404, never HTML).
func (s *StaticSite) Handler() http.Handler {
	fileServer := http.FileServer(http.FS(s.dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeProblem(w, r, errs.New(errs.NotFound, "not found"))
			return
		}
		path := r.URL.Path
		if isAPIPath(path) {
			writeProblem(w, r, errs.New(errs.NotFound, "no such API endpoint"))
			return
		}

		applyStaticHeaders(w, path)

		if path != "/" {
			if f, err := s.dist.Open(strings.TrimPrefix(path, "/")); err == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// SPA fallback: client-side routes get the shell.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(s.indexHTML)
	})
}

// applyStaticHeaders sets the deployment security and cache baseline
// (ADR-0007). The portal index ships inline bootstrap scripts (theme/locale
// pre-detection), hence unsafe-inline for scripts until the hardening
// follow-up lands nonces.
func applyStaticHeaders(w http.ResponseWriter, path string) {
	if strings.HasPrefix(path, "/assets/") {
		// Vite emits content-hashed filenames: immutable caching is safe.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// index.html and SPA routes must always revalidate.
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self' data:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
}
