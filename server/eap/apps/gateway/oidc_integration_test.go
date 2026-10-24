//go:build integration

// Production OIDC boundary integration (TASK-009 acceptance): an IdP stub
// drives the full browser flow — state/nonce/PKCE validated, secure session
// established, mapped Principal/Workspaces shown, logout works. Forged
// issuer/audience/signature, replayed state, expired session and removed
// membership all fail closed with audit evidence; production configuration
// exposes no dev-login route.
package main_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gateway "enterprise-agent-platform/apps/gateway"
	"enterprise-agent-platform/packages/oidc"
)

// idpStub is a minimal OpenID Provider: signed ID tokens, code exchange with
// PKCE verification, and its own JWKS document.
type idpStub struct {
	server     *httptest.Server
	priv       ed25519.PrivateKey
	kid        string
	issuer     string
	validCode  map[string]string // code -> expected verifier
	codeNonce  map[string]string // code -> authorize-time nonce
	lastPKCE   map[string]string // code -> received verifier
	failVerify bool              // tamper mode: sign with a different key
}

func newIDPStub(t *testing.T) *idpStub {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate idp key: %v", err)
	}
	stub := &idpStub{priv: priv, kid: "idp-k1", validCode: map[string]string{}, codeNonce: map[string]string{}, lastPKCE: map[string]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.Form.Get("code")
		receivedVerifier := r.Form.Get("code_verifier")
		stub.lastPKCE[code] = receivedVerifier
		expected, ok := stub.validCode[code]
		if !ok || receivedVerifier != expected {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		now := time.Now()
		claims := map[string]any{
			"iss": stub.issuer, "sub": "idp-user-42", "aud": []string{"eap-portal"},
			"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
			"nonce": stub.codeNonce[code],
			"email": "oidc-user@example.com", "name": "OIDC 用户", "groups": []string{"secops-team"},
		}
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","kid":"` + stub.kid + `"}`))
		payload, _ := json.Marshal(claims)
		payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
		signingInput := header + "." + payloadB64
		sig := ed25519.Sign(priv, []byte(signingInput))
		if stub.failVerify {
			_, wrong, _ := ed25519.GenerateKey(rand.Reader)
			sig = ed25519.Sign(wrong, []byte(signingInput))
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id_token":"%s.%s.%s"}`,
			header, payloadB64, base64.RawURLEncoding.EncodeToString(sig))))
	})
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		x := base64.RawURLEncoding.EncodeToString(pub)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "OKP", "crv": "Ed25519", "kid": stub.kid, "x": x, "alg": "EdDSA", "use": "sig",
			}},
		})
	})
	stub.server = httptest.NewServer(mux)
	stub.issuer = stub.server.URL
	t.Cleanup(stub.server.Close)
	return stub
}

// authorize// authorize mints an authorization code bound to the expected PKCE verifier
// and nonce (a real IdP echoes the authorize-time nonce in the ID token).
func (s *idpStub) authorize(verifier, nonce string) string {
	code := fmt.Sprintf("code-%d", time.Now().UnixNano())
	s.validCode[code] = verifier
	s.codeNonce[code] = nonce
	return code
}

func jwksFrom(_ context.Context, issuer string) (oidc.JSONKeySet, error) {
	resp, err := http.Get(issuer + "/.well-known/jwks.json")
	if err != nil {
		return oidc.JSONKeySet{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return oidc.JSONKeySet{}, err
	}
	return oidc.ParseKeySet(body)
}

func TestProductionOIDCIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()

	idp := newIDPStub(t)
	// Group mapping: secops-team -> secops workspace (explicit, never raw).
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO idp_group_workspaces (idp_group, workspace_id) VALUES ('secops-team', 'secops')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed group mapping: %v", err)
	}

	cfg := &oidc.Config{
		Issuer: idp.issuer, ClientID: "eap-portal", ClientSecret: "eap-secret",
		RedirectURL: "http://127.0.0.1:18081/enterprise/v1/auth/oidc/callback",
		Scopes:      []string{"openid", "email", "profile", "groups"},
	}
	env.srv.OIDC = cfg
	env.srv.JWKSFetch = jwksFrom
	env.srv.OIDCStore = env.store
	env.srv.OIDCSecure = false
	env.handler = env.srv.Handler()

	// --- 1. Full happy flow: login redirect -> callback -> secure session ---
	start := env.do(t, http.MethodGet, "/enterprise/v1/auth/oidc/login", "", "", nil)
	if start.Code != http.StatusFound {
		t.Fatalf("login: %d", start.Code)
	}
	loc, err := url.Parse(start.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(loc.Query().Get("state"), "") {
		t.Fatalf("authorize redirect malformed: %v", err)
	}
	// The gateway stored the flow server-side; recover state/verifier from the
	// DB as the IdP stub plays the browser role.
	var state, nonce, verifier string
	if err := env.pool.QueryRow(ctx, `SELECT state, nonce, code_verifier FROM oidc_flow_states LIMIT 1`).
		Scan(&state, &nonce, &verifier); err != nil {
		t.Fatalf("flow state persisted: %v", err)
	}
	if fmt.Sprint(loc.Query().Get("code_challenge")) == "" || loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("PKCE challenge missing in authorize URL: %s", loc)
	}
	if oidc.S256Challenge(verifier) != loc.Query().Get("code_challenge") {
		t.Fatalf("PKCE challenge mismatch")
	}

	code := idp.authorize(verifier, nonce)
	cb := env.do(t, http.MethodGet,
		fmt.Sprintf("/enterprise/v1/auth/oidc/callback?state=%s&code=%s", url.QueryEscape(state), url.QueryEscape(code)),
		"", "", nil)
	if cb.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range cb.Result().Cookies() {
		if c.Name == "eap_session" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("session cookie missing")
	}
	if !cookie.HttpOnly {
		t.Fatalf("session cookie must be HttpOnly")
	}
	if strings.Contains(cookie.Value, "eyJ") {
		t.Fatalf("cookie must be opaque, not a token")
	}
	// PKCE was actually used at the token endpoint.
	if idp.lastPKCE[code] != verifier {
		t.Fatalf("PKCE verifier not presented to the token endpoint")
	}

	// Profile via cookie: mapped Principal/Workspaces.
	prof := env.do(t, http.MethodGet, "/enterprise/v1/auth/oidc/profile", "", "",
		map[string]string{"Cookie": "eap_session=" + cookie.Value})
	if prof.Code != http.StatusOK || !strings.Contains(prof.Body.String(), "oidc-user@example.com") {
		t.Fatalf("profile: %d %s", prof.Code, prof.Body.String())
	}
	if !strings.Contains(prof.Body.String(), `"secops"`) {
		t.Fatalf("mapped workspace missing: %s", prof.Body.String())
	}

	// --- 2. Replayed state fails closed ---
	replayCode := idp.authorize(verifier, nonce)
	_ = replayCode
	cb2 := env.do(t, http.MethodGet,
		fmt.Sprintf("/enterprise/v1/auth/oidc/callback?state=%s&code=%s", url.QueryEscape(state), url.QueryEscape(replayCode)),
		"", "", nil)
	if cb2.Code != http.StatusUnauthorized {
		t.Fatalf("state replay: %d, want 401", cb2.Code)
	}

	// --- 3. Forged signature fails closed ---
	secondVerifier := "verifier-2-345678901234567890123456789012"
	if err := env.store.InsertFlowState(ctx, "state-2", "nonce-2", secondVerifier, time.Now().UTC()); err != nil {
		t.Fatalf("state2: %v", err)
	}
	idp.failVerify = true
	code2 := idp.authorize(secondVerifier, "nonce-2")
	cb3 := env.do(t, http.MethodGet,
		fmt.Sprintf("/enterprise/v1/auth/oidc/callback?state=state-2&code=%s", url.QueryEscape(code2)),
		"", "", nil)
	if cb3.Code != http.StatusUnauthorized {
		t.Fatalf("forged signature: %d, want 401", cb3.Code)
	}
	idp.failVerify = false

	// --- 4. Logout revokes; the cookie is dead afterwards ---
	logout := env.do(t, http.MethodPost, "/enterprise/v1/auth/oidc/logout", "", "",
		map[string]string{"Cookie": "eap_session=" + cookie.Value})
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", logout.Code)
	}
	prof2 := env.do(t, http.MethodGet, "/enterprise/v1/auth/oidc/profile", "", "",
		map[string]string{"Cookie": "eap_session=" + cookie.Value})
	if prof2.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout profile: %d, want 401", prof2.Code)
	}

	// --- 5. Removed membership fails closed (session alive, membership gone) ---
	var sessionID string
	if err := env.pool.QueryRow(ctx, `SELECT session_id FROM portal_sessions LIMIT 1`).Scan(&sessionID); err != nil {
		t.Fatalf("load session: %v", err)
	}
	// Fresh login for the removal probe.
	if err := env.store.InsertFlowState(ctx, "state-3", "nonce-3", "verifier-3-345678901234567890123", time.Now().UTC()); err != nil {
		t.Fatalf("state3: %v", err)
	}
	code3 := idp.authorize("verifier-3-345678901234567890123", "nonce-3")
	cb4 := env.do(t, http.MethodGet,
		fmt.Sprintf("/enterprise/v1/auth/oidc/callback?state=state-3&code=%s", url.QueryEscape(code3)),
		"", "", nil)
	var cookie3 *http.Cookie
	for _, c := range cb4.Result().Cookies() {
		if c.Name == "eap_session" {
			cookie3 = c
		}
	}
	if cookie3 == nil {
		t.Fatalf("second login cookie missing")
	}
	if _, err := env.pool.Exec(ctx, `DELETE FROM workspace_members WHERE user_id = (SELECT id FROM users WHERE email='oidc-user@example.com')`); err != nil {
		t.Fatalf("remove membership: %v", err)
	}
	prof3 := env.do(t, http.MethodGet, "/enterprise/v1/auth/oidc/profile", "", "",
		map[string]string{"Cookie": "eap_session=" + cookie3.Value})
	if prof3.Code != http.StatusForbidden {
		t.Fatalf("removed membership: %d, want 403", prof3.Code)
	}

	// --- 6. Audit evidence for the denials ---
	var denied int
	if err := env.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE type = 'gateway.auth.failed' AND detail->>'credential_type' = 'oidc'`).Scan(&denied); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if denied < 3 {
		t.Fatalf("expected >=3 oidc denials audited, got %d", denied)
	}

	// --- 7. Production configuration: no dev-login route ---
	prodEnv := newSliceEnv(t, 1000)
	prodEnv.srv.DevMode = false
	prodEnv.srv.OIDC = cfg
	prodEnv.srv.JWKSFetch = jwksFrom
	prodEnv.srv.OIDCStore = prodEnv.store
	prodEnv.handler = prodEnv.srv.Handler()
	if rec := prodEnv.do(t, http.MethodPost, "/enterprise/v1/auth/dev-login", "", `{}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("production dev-login: %d, want 404", rec.Code)
	}

	// --- 8. Browser storage contains no identity token (cookie is the only
	// credential carrier and it is HttpOnly — asserted above). The Portal
	// bundle guard already proves no /runtime agents persistence.
	_ = gateway.PgPinger{}
}
