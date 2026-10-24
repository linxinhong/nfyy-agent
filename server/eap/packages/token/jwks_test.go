package token_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"enterprise-agent-platform/internal/golden"
	"enterprise-agent-platform/packages/token"
)

func newTestKey(t *testing.T) token.VerificationKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return token.VerificationKey{KeyID: "k1", Public: priv.Public().(ed25519.PublicKey)}
}

func TestJWKSFixture(t *testing.T) {
	// The zero public key matches the published example fixture.
	zero := make(ed25519.PublicKey, ed25519.PublicKeySize)
	jwks, err := token.BuildJWKS(token.VerificationKey{KeyID: "k1", Public: zero})
	if err != nil {
		t.Fatalf("build jwks: %v", err)
	}
	golden.Check(t, "../../contracts/schemas/v1/enterprise/jwks.example.json", jwks)
}

func TestJWKSRoundTrip(t *testing.T) {
	key := newTestKey(t)
	jwks, err := token.BuildJWKS(key)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0].Alg != "EdDSA" || jwks.Keys[0].Use != "sig" {
		t.Fatalf("jwk fields wrong: %+v", jwks.Keys)
	}
	back, err := token.VerificationKeyFromJWK(jwks.Keys[0])
	if err != nil {
		t.Fatalf("from jwk: %v", err)
	}
	if string(back.Public) != string(key.Public) || back.KeyID != key.KeyID {
		t.Fatalf("round trip lost key data")
	}
}

func TestVerificationKeyFromJWKRejectsWrongTypes(t *testing.T) {
	key := newTestKey(t)
	jwk, err := token.PublicJWK(key)
	if err != nil {
		t.Fatalf("public jwk: %v", err)
	}
	bad := jwk
	bad.Kty = "RSA"
	if _, err := token.VerificationKeyFromJWK(bad); err == nil {
		t.Fatalf("RSA jwk must be rejected")
	}
	bad2 := jwk
	bad2.Kid = ""
	if _, err := token.VerificationKeyFromJWK(bad2); err == nil {
		t.Fatalf("missing kid must be rejected")
	}
	bad3 := jwk
	bad3.X = "not-base64!!"
	if _, err := token.VerificationKeyFromJWK(bad3); err == nil {
		t.Fatalf("bad x must be rejected")
	}
}

// Session semantics (ADR-0002): agent-gateway control-plane sessions carry no
// workspace; capability audiences still require it.
func TestSessionWithoutWorkspaceIssuedForAgentGatewayOnly(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	signer, err := token.NewEd25519Signer("k1", "https://agent.company.com/", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	verifier, err := token.NewEd25519Verifier(token.VerificationKey{KeyID: "k1", Public: priv.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	sessionOnlyPrincipal := token.IssueInput{
		Session:  session(), // session() includes a workspace
		Audience: token.AudienceAgentGateway,
	}
	// A session-shaped input with empty workspace must issue for agent-gateway...
	sessionOnlyPrincipal.Session.Workspace.WorkspaceID = ""
	signed, err := signer.Sign(sessionOnlyPrincipal)
	if err != nil {
		t.Fatalf("agent-gateway session without workspace must be issuable: %v", err)
	}
	claims, err := verifier.Verify(signed, token.AudienceAgentGateway)
	if err != nil {
		t.Fatalf("agent-gateway session without workspace must verify: %v", err)
	}
	if claims.WorkspaceID != "" {
		t.Fatalf("session carries no workspace authority")
	}

	// ...but the same claims for a capability audience must not.
	sessionOnlyPrincipal.Audience = token.AudienceLLMGateway
	if _, err := signer.Sign(sessionOnlyPrincipal); err == nil {
		t.Fatalf("capability token without workspace must be refused at issuance")
	}
}
