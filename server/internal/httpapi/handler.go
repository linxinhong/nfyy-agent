package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	spec "yuanpu.dev/server/api"
	"yuanpu.dev/server/internal/api"
	"yuanpu.dev/server/internal/database"
)

type Readiness interface {
	Ready(context.Context) (int, error)
}

func New(version string, db Readiness) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id [16]byte
		_, _ = rand.Read(id[:])
		requestID := hex.EncodeToString(id[:])
		w.Header().Set("X-Request-Id", requestID)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		known := r.URL.Path == "/v1/health" || r.URL.Path == "/v1/ready" || r.URL.Path == "/openapi.yaml"
		if !known {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Endpoint not found", requestID)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Use GET for this endpoint", requestID)
			return
		}
		switch r.URL.Path {
		case "/v1/health":
			writeJSON(w, http.StatusOK, api.HealthResponse{Status: "ok", Service: "yuanpu-server", Version: version, ApiVersion: 1})
		case "/v1/ready":
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
				writeError(w, http.StatusServiceUnavailable, code, message, requestID)
				return
			}
			writeJSON(w, http.StatusOK, api.ReadyResponse{Status: "ready", SchemaVersion: schemaVersion})
		case "/openapi.yaml":
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
			_, _ = w.Write(spec.OpenAPI)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	var body api.ErrorResponse
	body.Error.Code = code
	body.Error.Message = message
	body.Error.RequestId = requestID
	writeJSON(w, status, body)
}
