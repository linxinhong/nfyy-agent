package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yuanpu.dev/server/internal/api"
	"yuanpu.dev/server/internal/database"
)

type readinessFunc func(context.Context) (int, error)

func (f readinessFunc) Ready(ctx context.Context) (int, error) { return f(ctx) }

func TestPublicEndpoints(t *testing.T) {
	called := 0
	handler := New("test-version", readinessFunc(func(ctx context.Context) (int, error) {
		called++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("readiness has no deadline")
		}
		return 1, nil
	}))
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	var body api.HealthResponse
	if err := json.Unmarshal(health.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if health.Code != 200 || body.Version != "test-version" || body.ApiVersion != 1 || called != 0 {
		t.Fatalf("liveness must be independent of database: %d %+v calls=%d", health.Code, body, called)
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/v1/ready", nil))
	if ready.Code != 200 || called != 1 {
		t.Fatalf("readiness: %d calls=%d", ready.Code, called)
	}
	spec := httptest.NewRecorder()
	handler.ServeHTTP(spec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if spec.Code != 200 || !strings.Contains(spec.Body.String(), "operationId: getHealth") {
		t.Fatal("OpenAPI not served")
	}
	if health.Header().Get("X-Request-Id") == ready.Header().Get("X-Request-Id") {
		t.Fatal("request IDs reused")
	}
}

func TestFailureContracts(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, code string
		status                   int
		err                      error
	}{
		{"missing database", "GET", "/v1/ready", "DATABASE_NOT_CONFIGURED", 503, database.ErrNotConfigured},
		{"database down", "GET", "/v1/ready", "DATABASE_UNAVAILABLE", 503, database.ErrUnavailable},
		{"pending migration", "GET", "/v1/ready", "MIGRATIONS_REQUIRED", 503, database.ErrMigrations},
		{"private failure", "GET", "/v1/ready", "DATABASE_UNAVAILABLE", 503, errors.New("postgres://secret:password@host")},
		{"unknown route", "GET", "/v1/accounts", "NOT_FOUND", 404, nil},
		{"wrong method", "POST", "/v1/health", "METHOD_NOT_ALLOWED", 405, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := New("test", readinessFunc(func(context.Context) (int, error) { return 0, tc.err }))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			var body api.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.status || body.Error.Code != tc.code || body.Error.RequestId == "" || body.Error.RequestId != response.Header().Get("X-Request-Id") {
				t.Fatalf("unexpected error: %d %+v", response.Code, body)
			}
			if strings.Contains(response.Body.String(), "password") {
				t.Fatal("database credentials exposed")
			}
			if response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("cross-origin access enabled implicitly")
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("operational response can be cached")
			}
		})
	}
}
