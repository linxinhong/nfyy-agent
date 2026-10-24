package industry

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"

	"yuanpu.dev/server/internal/httpkit"
)

type HTTP struct {
	pack         Pack
	files        fs.FS
	directory    string
	authenticate func(http.Handler) http.Handler
}

func NewHTTP(pack Pack, files fs.FS, directory string, authenticate func(http.Handler) http.Handler) *HTTP {
	return &HTTP{pack: pack, files: files, directory: directory, authenticate: authenticate}
}

func (h *HTTP) Mount(router chi.Router) {
	router.Group(func(protected chi.Router) {
		protected.Use(h.authenticate)
		protected.Get("/v1/industry", func(w http.ResponseWriter, r *http.Request) {
			httpkit.WriteJSON(w, http.StatusOK, h.pack)
		})
		protected.Get("/v1/industry/templates/{type}", func(w http.ResponseWriter, r *http.Request) {
			markdown, err := Template(h.files, h.directory, chi.URLParam(r, "type"))
			if err != nil {
				httpkit.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "Knowledge template not found")
				return
			}
			httpkit.WriteJSON(w, http.StatusOK, map[string]string{"markdown": markdown})
		})
		protected.Post("/v1/industry/validate", func(w http.ResponseWriter, r *http.Request) {
			var input Submission
			if httpkit.DecodeJSON(r, &input, 256<<10) != nil {
				httpkit.WriteError(w, r, http.StatusBadRequest, "INVALID_INPUT", "Invalid knowledge submission")
				return
			}
			violations := h.pack.Validate(input)
			httpkit.WriteJSON(w, http.StatusOK, map[string]any{"valid": len(violations) == 0, "violations": violations})
		})
	})
}
