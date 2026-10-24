// Package playground embeds the AgentOS Playground UI.
package playground

import (
	"embed"
	"io"
	"net/http"
)

//go:embed index.html
var content embed.FS

// Handler serves the embedded Playground HTML at /playground and /.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/playground" && r.URL.Path != "/playground/" {
			http.NotFound(w, r)
			return
		}
		f, err := content.Open("index.html")
		if err != nil {
			http.Error(w, "playground not found", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, f)
	})
}
