package content

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"yuanpu.dev/server/internal/httpkit"
	"yuanpu.dev/server/internal/identity"
)

type HTTP struct {
	store        *Store
	authenticate func(http.Handler) http.Handler
}

func NewHTTP(store *Store, authenticate func(http.Handler) http.Handler) *HTTP {
	return &HTTP{store: store, authenticate: authenticate}
}

// Mount owns all content routes. The host supplies the same identity
// middleware used by the account API, keeping authorization on the server.
func (h *HTTP) Mount(router chi.Router) {
	router.Group(func(protected chi.Router) {
		if h.authenticate == nil {
			protected.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					httpkit.WriteError(w, r, 503, "IDENTITY_UNAVAILABLE", "Identity service is unavailable")
				})
			})
		} else {
			protected.Use(h.authenticate)
		}
		protected.Get("/v1/resources", h.list)
		protected.Post("/v1/resources", h.create)
		protected.Get("/v1/resources/mine", h.mine)
		protected.Get("/v1/resources/{id}", h.get)
		protected.Put("/v1/resources/{id}", h.edit)
		protected.Post("/v1/resources/{id}/submit", h.transition("submit"))
		protected.Post("/v1/resources/{id}/approve", h.transition("approve"))
		protected.Post("/v1/resources/{id}/reject", h.transition("reject"))
		protected.Post("/v1/resources/{id}/withdraw", h.transition("withdraw"))
		protected.Get("/v1/review/queue", h.reviewQueue)
	})
}

func (h *HTTP) mine(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	items, err := h.store.Mine(r.Context(), user)
	if err != nil {
		writeContentError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, items)
}

func writeContentError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid resource fields")
	case errors.Is(err, ErrForbidden):
		httpkit.WriteError(w, r, 403, "FORBIDDEN", "Resource action is not permitted")
	case errors.Is(err, ErrNotFound):
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "Resource not found")
	case errors.Is(err, ErrConflict):
		httpkit.WriteError(w, r, 409, "CONFLICT", "Resource state has changed")
	default:
		httpkit.WriteError(w, r, 503, "CONTENT_UNAVAILABLE", "Content service is unavailable")
	}
}

func (h *HTTP) current(w http.ResponseWriter, r *http.Request) (identity.User, bool) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		httpkit.WriteError(w, r, 401, "UNAUTHORIZED", "Authentication required or credentials invalid")
		return identity.User{}, false
	}
	if h.store == nil {
		httpkit.WriteError(w, r, 503, "CONTENT_UNAVAILABLE", "Content service is unavailable")
		return identity.User{}, false
	}
	return user, true
}

func resourceID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if !httpkit.ValidID(id) {
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "Resource not found")
		return "", false
	}
	return id, true
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	items, err := h.store.List(r.Context(), user, r.URL.Query().Get("kind"), r.URL.Query().Get("q"))
	if err != nil {
		writeContentError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, items)
}

func (h *HTTP) create(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	var input Input
	if httpkit.DecodeJSON(r, &input, 256<<10) != nil {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid resource request")
		return
	}
	item, err := h.store.Create(r.Context(), user, input)
	if err != nil {
		writeContentError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 201, item)
}

func (h *HTTP) get(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	id, ok := resourceID(w, r)
	if !ok {
		return
	}
	item, err := h.store.Get(r.Context(), user, id)
	if err != nil {
		writeContentError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, item)
}

func (h *HTTP) edit(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	id, ok := resourceID(w, r)
	if !ok {
		return
	}
	var input Input
	if httpkit.DecodeJSON(r, &input, 256<<10) != nil {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid resource request")
		return
	}
	item, err := h.store.Edit(r.Context(), user, id, input)
	if err != nil {
		writeContentError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, item)
}

func (h *HTTP) reviewQueue(w http.ResponseWriter, r *http.Request) {
	user, ok := h.current(w, r)
	if !ok {
		return
	}
	items, err := h.store.ReviewQueue(r.Context(), user)
	if err != nil {
		writeContentError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, items)
}

func (h *HTTP) transition(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := h.current(w, r)
		if !ok {
			return
		}
		id, ok := resourceID(w, r)
		if !ok {
			return
		}
		item, err := h.store.Transition(r.Context(), user, id, action)
		if err != nil {
			writeContentError(w, r, err)
			return
		}
		httpkit.WriteJSON(w, 200, item)
	}
}
