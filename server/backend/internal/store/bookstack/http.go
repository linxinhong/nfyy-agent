package bookstack

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"yuanpu.dev/server/internal/httpkit"
)

type HTTP struct {
	client       *Client
	authenticate func(http.Handler) http.Handler
}

func NewHTTP(client *Client, authenticate func(http.Handler) http.Handler) *HTTP {
	return &HTTP{client: client, authenticate: authenticate}
}

func (h *HTTP) Mount(router chi.Router) {
	router.Group(func(protected chi.Router) {
		protected.Use(h.authenticate)
		protected.Get("/v1/integrations/bookstack/status", func(w http.ResponseWriter, r *http.Request) {
			if err := h.client.Ping(r.Context()); err != nil {
				httpkit.WriteError(w, r, http.StatusServiceUnavailable, "BOOKSTACK_UNAVAILABLE", "BookStack connection is unavailable")
				return
			}
			httpkit.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		})
	})
}
