package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAuthMiddlewareDisabled(t *testing.T) {
	cfg := AuthorizationConfig{Enabled: false}
	mw := AuthMiddleware(cfg)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestAuthMiddlewareSecurityKey(t *testing.T) {
	cfg := AuthorizationConfig{Enabled: true, SecurityKey: "secret"}
	mw := AuthMiddleware(cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestAuthMiddlewareRejectsSecurityKeyWithUserIsolation(t *testing.T) {
	mw := AuthMiddleware(AuthorizationConfig{Enabled: true, UserIsolation: true, SecurityKey: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddlewareRequiresSubjectWithUserIsolation(t *testing.T) {
	const key = "hmac-secret"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	encoded, err := token.SignedString([]byte(key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	mw := AuthMiddleware(AuthorizationConfig{Enabled: true, UserIsolation: true, JWTVerificationKey: key})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+encoded)
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddlewareMissingHeader(t *testing.T) {
	cfg := AuthorizationConfig{Enabled: true}
	mw := AuthMiddleware(cfg)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddlewareJWT(t *testing.T) {
	key := "hmac-secret"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":    "user-1",
		"scopes": []string{"agent:run"},
		"exp":    time.Now().Add(time.Hour).Unix(),
	})
	tokenString, err := token.SignedString([]byte(key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	cfg := AuthorizationConfig{
		Enabled:            true,
		JWTVerificationKey: key,
	}
	mw := AuthMiddleware(cfg)

	var gotUserID string
	var gotScopes []string
	var gotIsolation bool
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserID = UserIDFromContext(r.Context())
		gotScopes = ScopesFromContext(r.Context())
		gotIsolation = UserIsolationFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotUserID != "user-1" {
		t.Fatalf("expected user-1, got %q", gotUserID)
	}
	if len(gotScopes) != 1 || gotScopes[0] != "agent:run" {
		t.Fatalf("unexpected scopes: %v", gotScopes)
	}
	if gotIsolation {
		t.Fatal("user isolation should be disabled by default")
	}
}

func TestAuthMiddlewareNamespaceClaim(t *testing.T) {
	const key = "tenant-key"
	for _, tc := range []struct {
		name  string
		claim any
		want  int
	}{
		{"missing is legacy compatible", nil, http.StatusOK},
		{"empty rejected", "", http.StatusUnauthorized},
		{"legacy rejected", "legacy", http.StatusUnauthorized},
		{"wrong type rejected", 7, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{"sub": "user", "exp": time.Now().Add(time.Hour).Unix()}
			if tc.claim != nil {
				claims["namespace"] = tc.claim
			}
			token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			encoded, err := token.SignedString([]byte(key))
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+encoded)
			rec := httptest.NewRecorder()
			AuthMiddleware(AuthorizationConfig{Enabled: true, JWTVerificationKey: key})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.claim == nil {
					if _, ok := NamespaceFromContext(r.Context()); ok {
						t.Fatal("missing claim installed namespace")
					}
				}
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
		})
	}
}

func TestAuthMiddlewareJWTInvalidSignature(t *testing.T) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "user-1",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	tokenString, err := token.SignedString([]byte("other-key"))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	cfg := AuthorizationConfig{
		Enabled:            true,
		JWTVerificationKey: "hmac-secret",
	}
	mw := AuthMiddleware(cfg)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddlewareRequiredScopes(t *testing.T) {
	key := "hmac-secret"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "user-1",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	tokenString, err := token.SignedString([]byte(key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	cfg := AuthorizationConfig{
		Enabled:            true,
		JWTVerificationKey: key,
		RequireScopes:      []string{"agent:run"},
	}
	mw := AuthMiddleware(cfg)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}
