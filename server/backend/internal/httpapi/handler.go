package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	spec "yuanpu.dev/server/api"
	"yuanpu.dev/server/internal/api"
	"yuanpu.dev/server/internal/database"
	"yuanpu.dev/server/internal/httpkit"
)

type Readiness interface {
	Ready(context.Context) (int, error)
}

type RouteModule interface {
	Mount(chi.Router)
}

// New owns only the HTTP host and public operational endpoints. Domain modules
// own their routes and are assembled by the application composition root.
func New(version string, db Readiness, modules ...RouteModule) http.Handler {
	router := chi.NewRouter()
	router.Use(httpkit.RequestMetadata)
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpkit.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "Endpoint not found")
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpkit.WriteError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed")
	})
	registerPublicRoutes(router, version, db)
	for _, module := range modules {
		if module != nil {
			module.Mount(router)
		}
	}
	return router
}

func registerPublicRoutes(router chi.Router, version string, db Readiness) {
	router.Get("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		httpkit.WriteJSON(w, http.StatusOK, api.HealthResponse{Status: "ok", Service: "yuanpu-server", Version: version, ApiVersion: 1})
	})
	router.Get("/v1/preview/wiki", func(w http.ResponseWriter, r *http.Request) {
		httpkit.WriteJSON(w, http.StatusOK, wikiPreviewResponse())
	})
	router.Get("/v1/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		schemaVersion, err := db.Ready(ctx)
		if err != nil {
			code, message := "DATABASE_UNAVAILABLE", "Database is unavailable"
			switch {
			case errors.Is(err, database.ErrNotConfigured):
				code, message = "DATABASE_NOT_CONFIGURED", "Database is not configured"
			case errors.Is(err, database.ErrMigrations):
				code, message = "MIGRATIONS_REQUIRED", "Run database migrations before using the server"
			}
			httpkit.WriteError(w, r, http.StatusServiceUnavailable, code, message)
			return
		}
		httpkit.WriteJSON(w, http.StatusOK, api.ReadyResponse{Status: "ready", SchemaVersion: schemaVersion})
	})
	router.Get("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(spec.OpenAPI)
	})
}
