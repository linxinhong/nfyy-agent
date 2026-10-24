package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"yuanpu.dev/server/internal/database"
)

func TestCompositionKeepsPublicAndProtectedRoutesSeparate(t *testing.T) {
	if _, err := NewHandler("test", nil); err == nil {
		t.Fatal("nil database store was accepted")
	}
	db, err := database.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler, err := NewHandler("test", db)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/health", 200},
		{"GET", "/v1/resources", 401},
		{"POST", "/v1/auth/api-key", 503},
		{"POST", "/v1/logs", 401},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.path == "/v1/auth/api-key" {
			request = httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"apiKey":"yp_test"}`))
			request.Header.Set("Content-Type", "application/json")
		}
		handler.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("%s %s: got %d, want %d", tc.method, tc.path, response.Code, tc.status)
		}
	}
}
