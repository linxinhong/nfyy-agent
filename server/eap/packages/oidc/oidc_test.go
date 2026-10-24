package oidc_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"enterprise-agent-platform/packages/oidc"
)

type fixedJWKS struct{ set oidc.JSONKeySet }

func (f fixedJWKS) fetch(_ context.Context, _ string) (oidc.JSONKeySet, error) { return f.set, nil }

func signIDToken(t *testing.T, priv ed25519.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","kid":"` + kid + `"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(header+"."+payloadB64))
	return header + "." + payloadB64 + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func testConfig() oidc.Config {
	return oidc.Config{Issuer: "https://idp.example.com", ClientID: "eap-portal",
		Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
}

func validClaims() map[string]any {
	return map[string]any{
		"iss": "https://idp.example.com", "sub": "u1", "aud": []string{"eap-portal"},
		"exp": 1_800_000_600, "nonce": "nonce-1", "email": "u@x.io", "groups": []string{"g1"},
	}
}

func TestValidateIDTokenHappyPath(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	set, _ := oidc.ParseKeySet(mustJWKS(t, pub))
	cfg := testConfig()
	res, err := oidc.ValidateIDToken(signIDToken(t, priv, "k1", validClaims()), "nonce-1", cfg, fixedJWKS{set}.fetch)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if res.Subject != "u1" || res.Email != "u@x.io" || len(res.Groups) != 1 {
		t.Fatalf("claims mapped wrong: %+v", res)
	}
}

func mustJWKS(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "kid": "k1",
		"x": base64.RawURLEncoding.EncodeToString(pub), "alg": "EdDSA", "use": "sig",
	}}})
	return b
}

func TestValidateIDTokenRejections(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	set, _ := oidc.ParseKeySet(mustJWKS(t, pub))
	cfg := testConfig()

	t.Run("wrong audience", func(t *testing.T) {
		claims := validClaims()
		claims["aud"] = []string{"other-client"}
		if _, err := oidc.ValidateIDToken(signIDToken(t, priv, "k1", claims), "nonce-1", cfg, fixedJWKS{set}.fetch); err == nil {
			t.Fatalf("wrong audience accepted")
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		claims := validClaims()
		claims["iss"] = "https://evil.example.com"
		if _, err := oidc.ValidateIDToken(signIDToken(t, priv, "k1", claims), "nonce-1", cfg, fixedJWKS{set}.fetch); err == nil {
			t.Fatalf("wrong issuer accepted")
		}
	})
	t.Run("expired", func(t *testing.T) {
		claims := validClaims()
		claims["exp"] = 1_799_000_000
		if _, err := oidc.ValidateIDToken(signIDToken(t, priv, "k1", claims), "nonce-1", cfg, fixedJWKS{set}.fetch); err == nil {
			t.Fatalf("expired accepted")
		}
	})
	t.Run("nonce mismatch", func(t *testing.T) {
		if _, err := oidc.ValidateIDToken(signIDToken(t, priv, "k1", validClaims()), "nonce-other", cfg, fixedJWKS{set}.fetch); err == nil {
			t.Fatalf("nonce mismatch accepted")
		}
	})
	t.Run("forged signature", func(t *testing.T) {
		_, wrong, _ := ed25519.GenerateKey(rand.Reader)
		if _, err := oidc.ValidateIDToken(signIDToken(t, wrong, "k1", validClaims()), "nonce-1", cfg, fixedJWKS{set}.fetch); err == nil {
			t.Fatalf("forged signature accepted")
		}
	})
	t.Run("unknown kid", func(t *testing.T) {
		if _, err := oidc.ValidateIDToken(signIDToken(t, priv, "k-other", validClaims()), "nonce-1", cfg, fixedJWKS{set}.fetch); err == nil {
			t.Fatalf("unknown kid accepted")
		}
	})
}
