package token_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/token"
)

func newKeys(t *testing.T) (ed25519.PrivateKey, token.Verifier, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier, err := token.NewEd25519Verifier(token.VerificationKey{KeyID: "k1", Public: pub})
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	return priv, verifier, "k1"
}

func newSigner(t *testing.T) (*token.Ed25519Signer, token.Verifier) {
	t.Helper()
	priv, verifier, kid := newKeys(t)
	signer, err := token.NewEd25519Signer(kid, "https://agent.company.com/", priv)
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	return signer, verifier
}

func session() identity.Session {
	return identity.Session{
		Principal: identity.Principal{
			UserID:   "user_123",
			Roles:    []string{"developer"},
			Scopes:   []string{"mcp:gitlab:read"},
			ClientID: "harness",
			DeviceID: "dev_1",
		},
		Workspace: identity.WorkspaceContext{WorkspaceID: "secops", Source: identity.WorkspaceSourceMembership, Roles: []string{"developer"}},
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	signer, verifier := newSigner(t)
	raw, err := signer.Sign(token.IssueInput{Session: session(), Audience: token.AudienceMCPGateway})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := verifier.Verify(raw, token.AudienceMCPGateway)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "user_123" || claims.WorkspaceID != "secops" || claims.Audience != token.AudienceMCPGateway {
		t.Fatalf("claims round trip lost data: %+v", claims)
	}
	if claims.JWTID == "" {
		t.Fatalf("jti is required")
	}
	if _, _, ok := strings.Cut(raw, "."); !ok {
		t.Fatalf("compact serialization expected")
	}
	s, err := claims.Session()
	if err != nil {
		t.Fatalf("claims must rebuild a valid session: %v", err)
	}
	if s.Workspace.Source != identity.WorkspaceSourceInternalJWT {
		t.Fatalf("jwt-sourced session must carry internal_jwt provenance")
	}
}

// Security regression: an internal token is only valid for its exact audience.
func TestInvalidAudienceRejected(t *testing.T) {
	signer, verifier := newSigner(t)
	raw, err := signer.Sign(token.IssueInput{Session: session(), Audience: token.AudienceLLMGateway})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := verifier.Verify(raw, token.AudienceMCPGateway); !errors.Is(err, token.ErrAudienceMismatch) {
		t.Fatalf("cross-audience use must be rejected with ErrAudienceMismatch, got %v", err)
	}
	if _, err := verifier.Verify(raw, "attacker-service"); !errors.Is(err, token.ErrAudienceMismatch) {
		t.Fatalf("unregistered expected audience must be rejected, got %v", err)
	}
	if _, err := signer.Sign(token.IssueInput{Session: session(), Audience: "attacker-service"}); err == nil {
		t.Fatalf("issuing for an unregistered audience must fail")
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	priv, verifier, kid := newKeys(t)
	signer, err := token.NewEd25519Signer(kid, "https://agent.company.com/", priv)
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	raw, err := signer.Sign(token.IssueInput{
		Session:  session(),
		Audience: token.AudienceKnowledge,
		Now:      time.Now().Add(-2 * token.MaxTTL),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := verifier.Verify(raw, token.AudienceKnowledge); !errors.Is(err, token.ErrExpired) {
		t.Fatalf("expired token must be rejected with ErrExpired, got %v", err)
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	signer, verifier := newSigner(t)
	raw, err := signer.Sign(token.IssueInput{Session: session(), Audience: token.AudienceLLMGateway})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parts := strings.Split(raw, ".")
	parts[1] += "x"
	if _, err := verifier.Verify(strings.Join(parts, "."), token.AudienceLLMGateway); err == nil {
		t.Fatalf("tampered token must be rejected")
	}
}

// Forged tokens must not pass by swapping the algorithm (alg=none / HS256).
func TestAlgorithmConfusionRejected(t *testing.T) {
	_, verifier, _ := newKeys(t)

	noneTok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "user_123", "aud": string(token.AudienceLLMGateway),
		token.ClaimWorkspace: "secops", "exp": time.Now().Add(time.Hour).Unix(),
	})
	noneRaw, err := noneTok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("build none token: %v", err)
	}
	if _, err := verifier.Verify(noneRaw, token.AudienceLLMGateway); err == nil {
		t.Fatalf("alg=none must be rejected")
	}

	hmacSecret := []byte("attacker-controlled-secret")
	hsTok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user_123", "aud": string(token.AudienceLLMGateway),
		token.ClaimWorkspace: "secops", "exp": time.Now().Add(time.Hour).Unix(),
	})
	hsRaw, err := hsTok.SignedString(hmacSecret)
	if err != nil {
		t.Fatalf("build hs256 token: %v", err)
	}
	if _, err := verifier.Verify(hsRaw, token.AudienceLLMGateway); err == nil {
		t.Fatalf("HS256 forgery must be rejected")
	}
}

func TestUnknownKeyIDRejected(t *testing.T) {
	priv, verifier, _ := newKeys(t)
	signer, err := token.NewEd25519Signer("attacker-kid", "https://agent.company.com/", priv)
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	raw, err := signer.Sign(token.IssueInput{Session: session(), Audience: token.AudienceLLMGateway})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := verifier.Verify(raw, token.AudienceLLMGateway); !errors.Is(err, token.ErrUnknownKeyID) {
		t.Fatalf("unknown kid must be rejected with ErrUnknownKeyID, got %v", err)
	}
}

func TestMissingWorkspaceClaimRejected(t *testing.T) {
	// A structurally valid token without workspace authority is invalid.
	priv, verifier, kid := newKeys(t)
	signer, err := token.NewEd25519Signer(kid, "https://agent.company.com/", priv)
	if err != nil {
		t.Fatalf("build signer: %v", err)
	}
	s := session()
	s.Workspace.WorkspaceID = ""
	if _, err := signer.Sign(token.IssueInput{Session: s, Audience: token.AudienceLLMGateway}); err == nil {
		t.Fatalf("issuing a token without workspace authority must fail")
	}

	// Craft a signed token that lacks the workspace claim entirely.
	noWs := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": "https://agent.company.com/", "sub": "user_123", "aud": string(token.AudienceLLMGateway),
		"exp": time.Now().Add(time.Minute).Unix(), "jti": "jti_1",
	})
	noWs.Header["kid"] = kid
	raw, err := noWs.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := verifier.Verify(raw, token.AudienceLLMGateway); !errors.Is(err, token.ErrMissingClaim) {
		t.Fatalf("token without %s claim must be rejected with ErrMissingClaim, got %v", token.ClaimWorkspace, err)
	}
}

func TestTTLClampedToMax(t *testing.T) {
	signer, verifier := newSigner(t)
	now := time.Now().UTC()
	raw, err := signer.Sign(token.IssueInput{
		Session: session(), Audience: token.AudienceLLMGateway,
		TTL: time.Hour, Now: now,
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := verifier.Verify(raw, token.AudienceLLMGateway)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	lifetime := claims.ExpiresAt.Sub(claims.IssuedAt)
	if lifetime != token.MaxTTL {
		t.Fatalf("lifetime %v must be clamped to %v", lifetime, token.MaxTTL)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	_, verifier, _ := newKeys(t)
	if _, err := verifier.Verify("not-a-jwt", token.AudienceLLMGateway); err == nil {
		t.Fatalf("garbage input must be rejected")
	}
}

// The kid header must be present for rotation to work.
func TestSignedTokenCarriesKID(t *testing.T) {
	signer, _ := newSigner(t)
	raw, err := signer.Sign(token.IssueInput{Session: session(), Audience: token.AudienceLLMGateway})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parts := strings.Split(raw, ".")
	headerJSON, err := jwt.NewParser().DecodeSegment(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var h map[string]any
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		t.Fatalf("header json: %v", err)
	}
	if h["alg"] != "EdDSA" {
		t.Fatalf("alg must be EdDSA, got %v", h["alg"])
	}
	if h["kid"] != "k1" {
		t.Fatalf("kid header missing: %v", h)
	}
}
