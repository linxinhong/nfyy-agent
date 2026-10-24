package token

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// JWK is one Ed25519 public key in JSON Web Key (OKP) form, as published by
// the gateway JWKS endpoint.
type JWK struct {
	Kty string `json:"kty"` // OKP
	Crv string `json:"crv"` // Ed25519
	Kid string `json:"kid"`
	X   string `json:"x"` // base64url raw public key
	Use string `json:"use"`
	Alg string `json:"alg"`
}

// JWKS is the JSON Web Key Set document served at /.well-known/jwks.json.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// PublicJWK converts one verification key into JWK form.
func PublicJWK(k VerificationKey) (JWK, error) {
	if len(k.Public) != ed25519.PublicKeySize {
		return JWK{}, fmt.Errorf("verification key %q is not an ed25519 public key", k.KeyID)
	}
	return JWK{
		Kty: "OKP",
		Crv: "Ed25519",
		Kid: k.KeyID,
		X:   base64.RawURLEncoding.EncodeToString(k.Public),
		Use: "sig",
		Alg: Algorithm,
	}, nil
}

// BuildJWKS renders the key set document for the given keys.
func BuildJWKS(keys ...VerificationKey) (JWKS, error) {
	out := JWKS{Keys: make([]JWK, 0, len(keys))}
	for _, k := range keys {
		jwk, err := PublicJWK(k)
		if err != nil {
			return JWKS{}, err
		}
		out.Keys = append(out.Keys, jwk)
	}
	return out, nil
}

// VerificationKeyFromJWK rebuilds a verification key from its JWK form,
// closing the JWKS -> Verifier round trip.
func VerificationKeyFromJWK(j JWK) (VerificationKey, error) {
	if j.Kty != "OKP" || j.Crv != "Ed25519" {
		return VerificationKey{}, fmt.Errorf("unsupported jwk type %s/%s", j.Kty, j.Crv)
	}
	if j.Kid == "" {
		return VerificationKey{}, fmt.Errorf("jwk is missing kid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(j.X)
	if err != nil {
		return VerificationKey{}, fmt.Errorf("jwk %q has invalid x: %w", j.Kid, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return VerificationKey{}, fmt.Errorf("jwk %q x has wrong length", j.Kid)
	}
	return VerificationKey{KeyID: j.Kid, Public: ed25519.PublicKey(raw)}, nil
}
