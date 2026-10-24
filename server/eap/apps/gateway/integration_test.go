//go:build integration

// Identity slice integration flow (TASK-003 acceptance): login, key creation,
// /enterprise/v1/me and downstream token exchange pass end to end against real
// PostgreSQL and Redis; revoked, expired, malformed and cross-workspace
// credentials fail closed with audit evidence in the database.
//
// Run via `make verify-identity-slice` (starts compose infra first).
package main_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	gateway "enterprise-agent-platform/apps/gateway"
	"enterprise-agent-platform/packages/apikey"
	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/mcpgateway"
	"enterprise-agent-platform/packages/store"
	"enterprise-agent-platform/packages/token"
	"enterprise-agent-platform/packages/tracing"
)

const testDBName = "eap_identity_test"

func integrationEnv(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("EAP_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("EAP_TEST_DATABASE_URL must point at a PostgreSQL admin database (make verify-identity-slice sets it)")
	}
	if os.Getenv("EAP_TEST_REDIS_ADDR") == "" {
		t.Fatal("EAP_TEST_REDIS_ADDR must point at Redis (make verify-identity-slice sets it)")
	}

	admin, err := pgxpool.New(context.Background(), adminURL)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(context.Background(),
		fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, testDBName)); err != nil {
		t.Fatalf("drop test db: %v", err)
	}
	if _, err := admin.Exec(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, testDBName)); err != nil {
		t.Fatalf("create test db: %v", err)
	}

	testURL := strings.Replace(adminURL, "/postgres?", "/"+testDBName+"?", 1)
	// Production pool sizing (TASK-040): the pgx default max(4, NumCPU)
	// would make concurrency measurements queue on a pool no deployment
	// actually runs.
	poolCfg, err := pgxpool.ParseConfig(testURL)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	poolCfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)

	migrationsDir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if err := store.Migrate(context.Background(), pool, migrationsDir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// sliceEnv bundles the server under test with its dependencies.
type sliceEnv struct {
	handler http.Handler
	srv     *gateway.Server
	store   *store.Store
	pool    *pgxpool.Pool
}

func newSliceEnv(t *testing.T, rateLimit int64) *sliceEnv {
	t.Helper()
	pool := integrationEnv(t)
	st := store.New(pool)

	signingKey, err := st.EnsureSigningKey(context.Background(), store.GenerateSigningKeyDER)
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}
	verifier, err := token.NewEd25519Verifier(token.VerificationKey{KeyID: signingKey.Kid, Public: signingKey.PublicKey})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	signer, err := token.NewEd25519Signer(signingKey.Kid, "https://agent.company.com/", signingKey.PrivateKey)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	jwksBody, err := json.Marshal(mustJWKS(t, token.VerificationKey{KeyID: signingKey.Kid, Public: signingKey.PublicKey}))
	if err != nil {
		t.Fatalf("jwks: %v", err)
	}

	srv := &gateway.Server{
		PG:        gateway.PgPinger{Pool: pool},
		Redis:     stubPinger{},
		Store:     st,
		Signer:    signer,
		Verifier:  verifier,
		JWKSBody:  jwksBody,
		AuthChain: st.GetAPIKeyAuthByHash, // same fused chain as production (TASK-043)
		Rate: &gateway.FixedWindow{
			Counter: gateway.RedisCounter{Client: redis.NewClient(&redis.Options{Addr: os.Getenv("EAP_TEST_REDIS_ADDR")})},
			Limit:   rateLimit,
			Window:  time.Minute,
		},
		DevMode: true,
		MCP: &mcpgateway.Service{
			Registry: st,
			Resolver: mcpgateway.EnvResolver{},
			Executor: &mcpgateway.HTTPExecutor{},
			Audit:    st,
		},
		Credential: func(ctx context.Context, ref string) (string, error) {
			return mcpgateway.EnvResolver{}.Resolve(ctx, ref)
		},
	}
	return &sliceEnv{handler: srv.Handler(), srv: srv, store: st, pool: pool}
}

func (e *sliceEnv) do(t *testing.T, method, path, bearer, jsonBody string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(jsonBody))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" { // binary uploads (blobs) set their own
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func TestIdentitySliceIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()

	// 1. Login (dev OIDC adapter = IdP stand-in) and receive a control-plane session.
	rec := env.do(t, http.MethodPost, "/enterprise/v1/auth/dev-login", "",
		`{"email":"li.lei@example.com","display_name":"Li Lei","workspaces":["secops"]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev-login: %d %s", rec.Code, rec.Body.String())
	}
	var session struct {
		SessionToken string `json:"session_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &session)
	if session.SessionToken == "" {
		t.Fatalf("session token missing: %s", rec.Body.String())
	}

	// 1b. Session-authenticated profile (Portal surface, TASK-005).
	rec = env.do(t, http.MethodGet, "/enterprise/v1/profile", session.SessionToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("profile: %d %s", rec.Code, rec.Body.String())
	}

	// 2. Create an employee API key; the full value is shown exactly once.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/keys", session.SessionToken,
		`{"name":"MacBook Harness","environment":"live"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		KeyID  string `json:"key_id"`
		Key    string `json:"key"`
		Prefix string `json:"prefix"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if !strings.HasPrefix(created.Key, "agk_live_") {
		t.Fatalf("one-time key missing: %+v", created)
	}
	if created.Prefix == created.Key {
		t.Fatalf("prefix must be a short form, not the full key")
	}

	// 3. Listing shows metadata only.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/keys", session.SessionToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), created.Key) {
		t.Fatalf("key listing must never contain the full key value")
	}

	// 4. /enterprise/v1/me with the agk key.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/me", created.Key, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d %s", rec.Code, rec.Body.String())
	}
	var me struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
		Workspaces       []struct{ ID string } `json:"workspaces"`
		DefaultWorkspace string                `json:"default_workspace"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.User.Email != "li.lei@example.com" || len(me.Workspaces) != 1 || me.Workspaces[0].ID != "secops" {
		t.Fatalf("me payload wrong: %s", rec.Body.String())
	}

	// 5. Token exchange for a downstream audience.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/token/exchange", created.Key,
		`{"audience":"mcp-gateway","ttl_seconds":600}`,
		map[string]string{identity.HeaderWorkspace: "secops"})
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", rec.Code, rec.Body.String())
	}
	var ex struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ex)
	if ex.AccessToken == "" || ex.TokenType != "Bearer" {
		t.Fatalf("exchange payload wrong: %s", rec.Body.String())
	}

	// 6. The token verifies for mcp-gateway and carries the workspace.
	// Cross-audience use must be rejected (TASK-001 contract holds e2e).
	verifier := env.verifier(t)
	claims, err := verifier.Verify(ex.AccessToken, "mcp-gateway")
	if err != nil {
		t.Fatalf("exchanged token must verify: %v", err)
	}
	if claims.WorkspaceID != "secops" || claims.Subject != me.User.ID {
		t.Fatalf("token claims wrong: %+v", claims)
	}
	if _, err := verifier.Verify(ex.AccessToken, "llm-gateway"); err == nil {
		t.Fatalf("cross-audience use must fail")
	}

	// 7. JWKS serves the signing key; a verifier built from JWKS verifies.
	rec = env.do(t, http.MethodGet, "/.well-known/jwks.json", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("jwks: %d", rec.Code)
	}
	var jwks struct {
		Keys []token.JWK `json:"keys"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &jwks)
	if len(jwks.Keys) != 1 {
		t.Fatalf("jwks must contain the active key: %s", rec.Body.String())
	}
	vk, err := token.VerificationKeyFromJWK(jwks.Keys[0])
	if err != nil {
		t.Fatalf("jwk -> verification key: %v", err)
	}
	jwksVerifier, err := token.NewEd25519Verifier(vk)
	if err != nil {
		t.Fatalf("jwks verifier: %v", err)
	}
	if _, err := jwksVerifier.Verify(ex.AccessToken, "mcp-gateway"); err != nil {
		t.Fatalf("token must verify via published JWKS: %v", err)
	}

	// 8. Cross-workspace credential fails closed with audit evidence.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/token/exchange", created.Key,
		`{"audience":"mcp-gateway"}`,
		map[string]string{identity.HeaderWorkspace: "finance"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace exchange: %d %s", rec.Code, rec.Body.String())
	}

	// 9. Revoke via the control plane.
	rec = env.do(t, http.MethodDelete, "/enterprise/v1/keys/"+created.KeyID, session.SessionToken, "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}

	// 10. The revoked key is rejected immediately and audited.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/me", created.Key, "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key me: %d, want 401", rec.Code)
	}

	// 11. Malformed credential fails closed and audited.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/me", "agk_live_tooshort", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("malformed key: %d, want 401", rec.Code)
	}

	// 12. Expired credential fails closed and audited (row planted directly).
	expired, err := apikey.Generate(apikey.EnvLive)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if _, err := env.store.CreateAPIKey(ctx, me.User.ID, apikey.Hash(expired.Full), expired.Prefix, "expired", "", &past,
		audit.Event{ID: "00000000-0000-0000-0000-0000000000ea", Type: audit.EventAuthKeyCreated, Time: time.Now().UTC(), Outcome: audit.OutcomeAllowed}); err != nil {
		t.Fatalf("plant expired key: %v", err)
	}
	rec = env.do(t, http.MethodGet, "/enterprise/v1/me", expired.Full, "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key: %d, want 401", rec.Code)
	}

	// 13. Audit evidence for every security event class is in PostgreSQL.
	assertAuditTypes(t, env.pool,
		"identity.key.created",
		"identity.key.revoked",
		"gateway.auth.failed", // revoked + malformed + expired + cross-workspace denials
		"gateway.token.exchanged",
	)

	// 14. Revoked key cannot exchange either.
	rec = env.do(t, http.MethodPost, "/enterprise/v1/token/exchange", created.Key,
		`{"audience":"mcp-gateway"}`,
		map[string]string{identity.HeaderWorkspace: "secops"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key exchange: %d, want 401", rec.Code)
	}

	// 15. Trace propagation holds on the enterprise surface too.
	req := httptest.NewRequest(http.MethodGet, "/debug/request", nil)
	req.Header.Set(tracing.HeaderTraceparent, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec = httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if got := rec.Header().Get(tracing.HeaderTraceparent); got != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatalf("inbound traceparent must be echoed, got %q", got)
	}
}

// Rate limiting runs on the live Redis counter; the third request inside the
// window is denied with 429.
func TestIdentitySliceRateLimitIntegration(t *testing.T) {
	env := newSliceEnv(t, 2)

	if rec := env.do(t, http.MethodPost, "/enterprise/v1/auth/dev-login", "",
		`{"email":"rl@example.com","workspaces":["secops"]}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("login: %d", rec.Code)
	}
	var user struct {
		ID string `json:"user_id"`
	}
	// Resolve the synced user id from the store (login re-synced it).
	if err := env.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'rl@example.com'`).Scan(&user.ID); err != nil {
		t.Fatalf("load user: %v", err)
	}
	keyValue := seedKey(t, env, user.ID)

	for i := 0; i < 2; i++ {
		if rec := env.do(t, http.MethodGet, "/enterprise/v1/me", keyValue, "", nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i+1, rec.Code)
		}
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/me", keyValue, "", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: %d, want 429", rec.Code)
	}
}

// --- helpers ---

func seedKey(t *testing.T, env *sliceEnv, userID string) string {
	t.Helper()
	g, err := apikey.Generate(apikey.EnvLive)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	ev := audit.NewEvent(audit.EventAuthKeyCreated, audit.OutcomeAllowed)
	if _, err := env.store.CreateAPIKey(context.Background(), userID, apikey.Hash(g.Full), g.Prefix, "seeded", "", nil, ev); err != nil {
		t.Fatalf("plant key: %v", err)
	}
	return g.Full
}

func (e *sliceEnv) verifier(t *testing.T) token.Verifier {
	t.Helper()
	var kid string
	var der []byte
	if err := e.pool.QueryRow(context.Background(),
		`SELECT kid, private_pkcs8 FROM signing_keys WHERE active LIMIT 1`).Scan(&kid, &der); err != nil {
		t.Fatalf("load signing key: %v", err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatalf("parse signing key: %v", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("signing key is not ed25519")
	}
	v, err := token.NewEd25519Verifier(token.VerificationKey{KeyID: kid, Public: priv.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return v
}

func assertAuditTypes(t *testing.T, pool *pgxpool.Pool, types ...string) {
	t.Helper()
	for _, typ := range types {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_events WHERE type = $1`, typ).Scan(&n); err != nil {
			t.Fatalf("audit count %s: %v", typ, err)
		}
		if n == 0 {
			t.Errorf("audit evidence missing for %s", typ)
		}
	}
}

func mustJWKS(t *testing.T, key token.VerificationKey) token.JWKS {
	t.Helper()
	jwks, err := token.BuildJWKS(key)
	if err != nil {
		t.Fatalf("build jwks: %v", err)
	}
	return jwks
}
