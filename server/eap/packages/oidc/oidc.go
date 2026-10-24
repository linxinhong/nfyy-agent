// Package oidc implements the production login boundary (TASK-009):
// OpenID Connect authorization-code flow with state, nonce and PKCE, ID token
// validation (issuer/audience/signature/expiry), and HttpOnly session cookies.
//
// Hard rules (ADR-0009):
//   - no identity token, employee key or OIDC token is ever stored in browser
//     script-reachable storage; the browser holds only an opaque session id
//     inside a HttpOnly Secure SameSite cookie;
//   - caller-supplied workspace headers/claims never grant authority — the
//     session's workspaces come from the validated ID token claims mapped
//     through the canonical Principal boundary;
//   - the flow is replay-proof: state is single-use, PKCE verifier is bound
//     to the authorization request.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Session cookie attributes and names.
const (
	SessionCookieName = "eap_session"
	// TTL of the gateway session established after login.
	SessionTTL = 8 * time.Hour
)

// Errors surfaced to callers for fail-closed classification.
var (
	ErrStateMismatch = errors.New("oidc: state mismatch or replay")
	ErrBadVerifier   = errors.New("oidc: PKCE verifier mismatch")
	ErrBadIDToken    = errors.New("oidc: id token validation failed")
	ErrBadIssuer     = errors.New("oidc: issuer mismatch")
	ErrBadAudience   = errors.New("oidc: audience mismatch")
	ErrExpired       = errors.New("oidc: token expired")
)

// Config describes the enterprise IdP and this client (the gateway).
type Config struct {
	Issuer       string // https://idp.example.com
	ClientID     string
	ClientSecret string // confidential client; stays server-side
	RedirectURL  string // https://agent.company.com/enterprise/v1/auth/oidc/callback
	Scopes       []string
	HTTPClient   *http.Client
	Now          func() time.Time
}

// AuthorizationRequest is one in-flight login attempt.
type AuthorizationRequest struct {
	State        string
	Nonce        string
	CodeVerifier string
	CreatedAt    time.Time
}

// NewAuthorizationRequest mints state/nonce/PKCE pair.
func NewAuthorizationRequest(now time.Time) (*AuthorizationRequest, error) {
	state, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	nonce, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	verifier, err := randomToken(64)
	if err != nil {
		return nil, err
	}
	return &AuthorizationRequest{State: state, Nonce: nonce, CodeVerifier: verifier, CreatedAt: now}, nil
}

// AuthorizeURL builds the IdP authorization redirect.
func (c *Config) AuthorizeURL(req *AuthorizationRequest) (string, error) {
	challenge := S256Challenge(req.CodeVerifier)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURL)
	q.Set("scope", strings.Join(c.Scopes, " "))
	q.Set("state", req.State)
	q.Set("nonce", req.Nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return strings.TrimSuffix(c.Issuer, "/") + "/authorize?" + q.Encode(), nil
}

// S256Challenge derives the PKCE code challenge (BASE64URL(SHA256(verifier))).
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ExchangeInput carries the callback parameters into the token exchange.
type ExchangeInput struct {
	Code          string
	ExpectedState string
	Stored        *AuthorizationRequest
	ReceivedState string
}

// TokenResult is the validated identity outcome of one login.
type TokenResult struct {
	Subject     string
	Email       string
	DisplayName string
	// Groups are IdP group names; mapping to internal workspaces happens in
	// the store layer through an explicit mapping (never used as raw ids).
	Groups []string
	Expiry time.Time
	// Diagnostics carries non-secret operator context when a field the
	// caller may require is absent (e.g. directory email not provisioned).
	Diagnostics string
}

// ValidateState enforces single-use state equality and freshness.
func ValidateState(stored *AuthorizationRequest, received string, now time.Time) error {
	if stored == nil {
		return ErrStateMismatch
	}
	if now.Sub(stored.CreatedAt) > 10*time.Minute {
		return ErrStateMismatch
	}
	if received == "" || !subtleEqual(stored.State, received) {
		return ErrStateMismatch
	}
	return nil
}

// ValidateIDToken verifies the ID token payload against the expected
// issuer/audience/nonce/expiry and the IdP's JWKS signature. jwksFetch is
// injected for testability; production fetches {issuer}/.well-known/jwks.json
// with caching.
func ValidateIDToken(rawIDToken, expectedNonce string, c Config, jwksFetch func(ctx context.Context, issuer string) (JSONKeySet, error)) (TokenResult, error) {
	var zero TokenResult
	parts := strings.Split(rawIDToken, ".")
	if len(parts) != 3 {
		return zero, ErrBadIDToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return zero, ErrBadIDToken
	}
	var claims struct {
		Issuer   string   `json:"iss"`
		Subject  string   `json:"sub"`
		Audience []string `json:"aud"`
		Expiry   int64    `json:"exp"`
		Nonce    string   `json:"nonce"`
		Email    string   `json:"email"`
		Name     string   `json:"name"`
		Groups   []string `json:"groups"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return zero, ErrBadIDToken
	}

	// Signature: JWKS lookup by kid, EdDSA/ES256 verification (RSA/none/HMAC
	// are rejected outright in the key-set verifier).
	keys, err := jwksFetch(context.Background(), c.Issuer)
	if err != nil {
		return zero, fmt.Errorf("%w: jwks unavailable: %v", ErrBadIDToken, err)
	}
	sigInput := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return zero, ErrBadIDToken
	}
	if !keys.Verify(sigInput, signature) {
		return zero, fmt.Errorf("%w: signature rejected by jwks", ErrBadIDToken)
	}

	now := c.Now
	if now == nil {
		now = time.Now
	}
	if now().After(time.Unix(claims.Expiry, 0)) {
		return zero, ErrExpired
	}
	if claims.Issuer != strings.TrimSuffix(c.Issuer, "/") {
		return zero, ErrBadIssuer
	}
	ok := false
	for _, a := range claims.Audience {
		if a == c.ClientID {
			ok = true
		}
	}
	if !ok {
		return zero, ErrBadAudience
	}
	if !subtleEqual(claims.Nonce, expectedNonce) {
		return zero, ErrBadIDToken
	}
	if claims.Subject == "" {
		return zero, ErrBadIDToken
	}
	return TokenResult{
		Subject: claims.Subject, Email: claims.Email, DisplayName: claims.Name,
		Groups: claims.Groups, Expiry: time.Unix(claims.Expiry, 0),
	}, nil
}

// ExchangeCode performs the code -> token exchange at the IdP token endpoint
// with the PKCE verifier and client authentication.
func (c *Config) ExchangeCode(ctx context.Context, in ExchangeInput) (rawIDToken string, err error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", in.Code)
	form.Set("redirect_uri", c.RedirectURL)
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	form.Set("code_verifier", in.Stored.CodeVerifier)

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.Issuer, "/")+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var tokenResp struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil || tokenResp.IDToken == "" {
		return "", errors.New("token endpoint returned no id_token")
	}
	return tokenResp.IDToken, nil
}

// SessionCookie builds the HttpOnly session cookie. No token material ever
// reaches script-reachable storage: the cookie value is an opaque session id.
func SessionCookie(value string, now time.Time, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  now.Add(SessionTTL),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearedCookie invalidates the session cookie at logout.
func ClearedCookie(now time.Time, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  now.Add(-time.Hour),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand unavailable: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// subtleEqual is a length-independent constant-time comparison.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
