// Package middleware provides HTTP middleware for AgentOS.
package middleware

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// AuthorizationConfig controls JWT and security-key authentication.
type AuthorizationConfig struct {
	Enabled            bool
	JWTVerificationKey string
	RequireScopes      []string
	UserIsolation      bool
	SecurityKey        string
}

// Context keys for request-scoped auth values.
type ctxKey int

const (
	userIDKey ctxKey = iota
	scopesKey
	userIsolationKey
	namespaceKey
	issuerKey
)

// UserIDFromContext returns the authenticated user ID, if any.
func UserIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(userIDKey).(string); ok {
		return id
	}
	return ""
}

// IssuerFromContext returns the issuer from the verified JWT.
func IssuerFromContext(ctx context.Context) string {
	issuer, _ := ctx.Value(issuerKey).(string)
	return issuer
}

// NamespaceFromContext returns the namespace from a verified JWT claim. A
// missing claim deliberately remains distinguishable for legacy compatibility.
func NamespaceFromContext(ctx context.Context) (string, bool) {
	namespace, ok := ctx.Value(namespaceKey).(string)
	return namespace, ok
}

// ScopesFromContext returns the scopes parsed from the JWT, if any.
func ScopesFromContext(ctx context.Context) []string {
	if s, ok := ctx.Value(scopesKey).([]string); ok {
		return s
	}
	return nil
}

// UserIsolationFromContext reports whether handlers must restrict resources to
// the authenticated subject.
func UserIsolationFromContext(ctx context.Context) bool {
	enabled, _ := ctx.Value(userIsolationKey).(bool)
	return enabled
}

// AuthMiddleware returns an http.Handler that enforces the configured auth rules.
func AuthMiddleware(cfg AuthorizationConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			token := parts[1]

			// Security-key authentication has no user subject, so it cannot be
			// used when resources must be isolated by user. Comparison is
			// constant-time to avoid a timing side channel on the key.
			if cfg.SecurityKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(cfg.SecurityKey)) == 1 {
				if cfg.UserIsolation {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			if cfg.JWTVerificationKey == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			claims, err := parseJWT(token, cfg.JWTVerificationKey)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if cfg.UserIsolation && claims.Subject == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if claims.NamespaceSet {
				if err := execution.ValidateNamespace(claims.Namespace); err != nil {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				if err := execution.ValidateNamespaceLength(claims.Namespace); err != nil {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}

			if missing := hasRequiredScopes(claims.Scopes, cfg.RequireScopes); len(missing) > 0 {
				http.Error(w, "forbidden: insufficient scopes", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), userIDKey, claims.Subject)
			ctx = context.WithValue(ctx, scopesKey, claims.Scopes)
			ctx = context.WithValue(ctx, userIsolationKey, cfg.UserIsolation)
			if claims.NamespaceSet {
				ctx = context.WithValue(ctx, namespaceKey, claims.Namespace)
				ctx = execution.ContextWithNamespace(ctx, claims.Namespace)
			}
			if claims.Issuer != "" {
				ctx = context.WithValue(ctx, issuerKey, claims.Issuer)
			}
			if claims.Subject != "" && claims.NamespaceSet && claims.Issuer != "" {
				ctx = service.ContextWithPrincipal(ctx, execution.Principal{Subject: claims.Subject, Namespace: claims.Namespace, Issuer: claims.Issuer})
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type jwtClaims struct {
	Subject      string   `json:"sub"`
	Scopes       []string `json:"scopes"`
	Namespace    string
	NamespaceSet bool
	Issuer       string
}

func parseJWT(token string, key string) (*jwtClaims, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(key), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, jwt.ErrTokenInvalidClaims
	}

	claims := &jwtClaims{}
	if sub, err := parsed.Claims.GetSubject(); err == nil {
		claims.Subject = sub
	}
	if issuer, err := parsed.Claims.GetIssuer(); err == nil {
		claims.Issuer = issuer
	}
	if raw, ok := parsed.Claims.(jwt.MapClaims)["scopes"]; ok {
		switch v := raw.(type) {
		case []string:
			claims.Scopes = v
		case []any:
			for _, s := range v {
				if str, ok := s.(string); ok {
					claims.Scopes = append(claims.Scopes, str)
				}
			}
		case string:
			claims.Scopes = strings.Fields(v)
		}
	}
	if raw, ok := parsed.Claims.(jwt.MapClaims)["namespace"]; ok {
		namespace, ok := raw.(string)
		if !ok {
			return nil, jwt.ErrTokenInvalidClaims
		}
		claims.Namespace = namespace
		claims.NamespaceSet = true
	}
	return claims, nil
}

func hasRequiredScopes(have, need []string) []string {
	if len(need) == 0 {
		return nil
	}
	haveSet := make(map[string]struct{}, len(have))
	for _, s := range have {
		haveSet[s] = struct{}{}
	}
	var missing []string
	for _, s := range need {
		if _, ok := haveSet[s]; !ok {
			missing = append(missing, s)
		}
	}
	return missing
}
