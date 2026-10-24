package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"yuanpu.dev/server/internal/httpkit"
)

type principalKey struct{}

type HTTP struct {
	store *Store
}

func NewHTTP(store *Store) *HTTP { return &HTTP{store: store} }

// Mount keeps identity route ownership in the identity module. The host
// supplies only the shared router and HTTP response contract.
func (h *HTTP) Mount(router chi.Router) {
	router.Get("/v1/portal/context", h.portalContext)
	router.Post("/v1/auth/password", h.loginWithPassword)
	router.Post("/v1/auth/api-key", h.loginWithAPIKey)
	router.Group(func(protected chi.Router) {
		protected.Use(h.Authenticate)
		protected.Get("/v1/auth/me", h.me)
		protected.Post("/v1/auth/logout", h.logout)
		protected.Post("/v1/auth/device-key", h.issueDeviceKey)
		protected.Group(func(admin chi.Router) {
			admin.Use(requireAdmin)
			admin.Get("/v1/admin/teams", h.listTeams)
			admin.Post("/v1/admin/teams", h.createTeam)
			admin.Get("/v1/admin/users", h.listUsers)
			admin.Post("/v1/admin/users", h.createUser)
			admin.Put("/v1/admin/users/{id}", h.updateUser)
			admin.Put("/v1/admin/users/{id}/password", h.setPassword)
			admin.Get("/v1/admin/roles", h.listRolePermissions)
			admin.Put("/v1/admin/roles/{role}/{action}", h.updateRolePermission)
			admin.Get("/v1/admin/api-keys", h.listAPIKeys)
			admin.Post("/v1/admin/api-keys", h.issueAPIKey)
			admin.Delete("/v1/admin/api-keys/{id}", h.revokeAPIKey)
		})
	})
}

func (h *HTTP) portalContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpkit.WriteJSON(w, 200, struct {
		AgentBrowser bool `json:"agentBrowser"`
	}{strings.Contains(r.UserAgent(), "YuanpuAgentDesktop/")})
}

// Authenticate is also the content module's channel boundary. Each request
// resolves the current session, so account deactivation takes effect at once.
func (h *HTTP) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.store == nil {
			httpkit.WriteError(w, r, 503, "IDENTITY_UNAVAILABLE", "Identity service is unavailable")
			return
		}
		token := httpkit.BearerToken(r)
		if token == "" {
			httpkit.WriteError(w, r, 401, "UNAUTHORIZED", "Authentication required or credentials invalid")
			return
		}
		user, err := h.store.SessionUser(r.Context(), token)
		if err != nil {
			writeIdentityError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, user)))
	})
}

func UserFromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(principalKey{}).(User)
	return user, ok
}

func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFromContext(r.Context())
		if !ok || user.Role != "admin" {
			httpkit.WriteError(w, r, 403, "FORBIDDEN", "Administrator role required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeIdentityError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		httpkit.WriteError(w, r, 401, "UNAUTHORIZED", "Authentication required or credentials invalid")
	case errors.Is(err, ErrInvalid):
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid account or team fields")
	case errors.Is(err, ErrConflict):
		httpkit.WriteError(w, r, 409, "CONFLICT", "Account or team already exists or has an invalid reference")
	case errors.Is(err, ErrNotFound):
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "Member or API key not found")
	default:
		httpkit.WriteError(w, r, 503, "IDENTITY_UNAVAILABLE", "Identity service is unavailable")
	}
}

func (h *HTTP) loginWithAPIKey(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		httpkit.WriteError(w, r, 503, "IDENTITY_UNAVAILABLE", "Identity service is unavailable")
		return
	}
	var input struct {
		APIKey string `json:"apiKey"`
	}
	if httpkit.DecodeJSON(r, &input, 8192) != nil || len(input.APIKey) > 256 {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid API key request")
		return
	}
	user, token, err := h.store.LoginWithAPIKey(r.Context(), input.APIKey)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, struct {
		User  User   `json:"user"`
		Token string `json:"token"`
	}{user, token})
}

func (h *HTTP) loginWithPassword(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		httpkit.WriteError(w, r, 503, "IDENTITY_UNAVAILABLE", "Identity service is unavailable")
		return
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if httpkit.DecodeJSON(r, &input, 4096) != nil || len(input.Email) > 254 || len(input.Password) > 1024 {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid login request")
		return
	}
	user, token, err := h.store.LoginWithPassword(r.Context(), input.Email, input.Password)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpkit.WriteJSON(w, 200, struct {
		User  User   `json:"user"`
		Token string `json:"token"`
	}{user, token})
}

func (h *HTTP) issueDeviceKey(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	key, secret, err := h.store.IssueOwnAPIKey(r.Context(), actor, httpkit.BearerToken(r))
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpkit.WriteJSON(w, 201, struct {
		APIKey APIKey `json:"apiKey"`
		Secret string `json:"secret"`
	}{key, secret})
}

func (h *HTTP) me(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	httpkit.WriteJSON(w, 200, user)
}

func (h *HTTP) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.store.RevokeSession(r.Context(), httpkit.BearerToken(r)); err != nil {
		writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) listTeams(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	teams, err := h.store.ListTeams(r.Context(), user)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, teams)
}

func (h *HTTP) createTeam(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if httpkit.DecodeJSON(r, &input, 8192) != nil {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid team request")
		return
	}
	user, _ := UserFromContext(r.Context())
	team, err := h.store.CreateTeam(r.Context(), user, input.Name)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 201, team)
}

func (h *HTTP) listUsers(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	members, err := h.store.ListUsers(r.Context(), user)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, members)
}

func (h *HTTP) createUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email  string  `json:"email"`
		Name   string  `json:"name"`
		Role   string  `json:"role"`
		TeamID *string `json:"teamId"`
	}
	if httpkit.DecodeJSON(r, &input, 8192) != nil {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid account request")
		return
	}
	user, _ := UserFromContext(r.Context())
	created, err := h.store.CreateUser(r.Context(), user, input.Email, input.Name, input.Role, input.TeamID)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 201, struct {
		User User `json:"user"`
	}{created})
}

func (h *HTTP) updateUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !httpkit.ValidID(id) {
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "Member not found")
		return
	}
	var input struct {
		Role   string  `json:"role"`
		TeamID *string `json:"teamId"`
		Active *bool   `json:"active"`
	}
	if httpkit.DecodeJSON(r, &input, 8192) != nil || input.Active == nil {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid account update")
		return
	}
	user, _ := UserFromContext(r.Context())
	updated, err := h.store.UpdateUser(r.Context(), user, id, input.Role, input.TeamID, *input.Active)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, updated)
}

func (h *HTTP) setPassword(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !httpkit.ValidID(id) {
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "Member not found")
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if httpkit.DecodeJSON(r, &input, 4096) != nil {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid password request")
		return
	}
	actor, _ := UserFromContext(r.Context())
	if err := h.store.SetUserPassword(r.Context(), actor, id, input.Password); err != nil {
		writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) listRolePermissions(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	items, err := h.store.ListRolePermissions(r.Context(), actor)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, items)
}

func (h *HTTP) updateRolePermission(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Scope string `json:"scope"`
	}
	if httpkit.DecodeJSON(r, &input, 8192) != nil {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid role permission")
		return
	}
	actor, _ := UserFromContext(r.Context())
	item, err := h.store.UpdateRolePermission(r.Context(), actor, chi.URLParam(r, "role"), chi.URLParam(r, "action"), input.Scope)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, item)
}

func (h *HTTP) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	keys, err := h.store.ListAPIKeys(r.Context(), actor)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 200, keys)
}

func (h *HTTP) issueAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UserID        string `json:"userId"`
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expiresInDays"`
	}
	if httpkit.DecodeJSON(r, &input, 8192) != nil || !httpkit.ValidID(input.UserID) {
		httpkit.WriteError(w, r, 400, "INVALID_INPUT", "Invalid API key request")
		return
	}
	actor, _ := UserFromContext(r.Context())
	key, secret, err := h.store.IssueAPIKey(r.Context(), actor, input.UserID, input.Name, input.ExpiresInDays)
	if err != nil {
		writeIdentityError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, 201, struct {
		APIKey APIKey `json:"apiKey"`
		Secret string `json:"secret"`
	}{key, secret})
}

func (h *HTTP) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !httpkit.ValidID(id) {
		httpkit.WriteError(w, r, 404, "NOT_FOUND", "API key not found")
		return
	}
	actor, _ := UserFromContext(r.Context())
	if err := h.store.RevokeAPIKey(r.Context(), actor, id); err != nil {
		writeIdentityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
