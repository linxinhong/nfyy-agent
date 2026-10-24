package oidc

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// JSONKey is one IdP public key in JWK form. EdDSA(Ed25519) and ES256 are
// supported — the algorithms the platform standardizes on.
type JSONKey struct {
	Kty string `json:"kty"`
	Crv string `json:"crv,omitempty"`
	Kid string `json:"kid"`
	Alg string `json:"alg,omitempty"`
	N   string `json:"n,omitempty"` // RSA modulus (rejected; see Verify)
	E   string `json:"e,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// JSONKeySet is the IdP JWKS document.
type JSONKeySet struct {
	Keys []JSONKey `json:"keys"`
}

// Verify checks the compact JWS signature over signingInput against the key
// set. Unknown kids, weak algorithms (none, HS256, RSA) and bad signatures
// all fail closed.
func (set JSONKeySet) Verify(signingInput string, signature []byte) bool {
	header := struct {
		Kid string `json:"kid"`
		Alg string `json:"alg"`
	}{}
	rawHeader, err := base64.RawURLEncoding.DecodeString(strings.Split(signingInput, ".")[0])
	if err != nil {
		return false
	}
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return false
	}
	for _, key := range set.Keys {
		if key.Kid != header.Kid {
			continue
		}
		switch {
		case key.Kty == "OKP" && (key.Alg == "EdDSA" || header.Alg == "EdDSA"):
			return verifyEd25519(key, signingInput, signature)
		case key.Kty == "EC" && (key.Alg == "ES256" || header.Alg == "ES256"):
			return verifyES256(key, signingInput, signature)
		default:
			// RSA/none/HMAC are never accepted for identity tokens.
			return false
		}
	}
	return false
}

func verifyEd25519(key JSONKey, signingInput string, signature []byte) bool {
	raw, err := base64.RawURLEncoding.DecodeString(key.X)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(raw), []byte(signingInput), signature)
}

func verifyES256(key JSONKey, signingInput string, signature []byte) bool {
	if key.Crv != "" && key.Crv != "P-256" {
		return false
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(key.X)
	if err != nil {
		return false
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(key.Y)
	if err != nil {
		return false
	}
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}
	if len(signature) != 64 {
		return false
	}
	// JWS ES256 signatures are r||s, each 32 bytes big-endian.
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	digest := sha256.Sum256([]byte(signingInput))
	return ecdsa.Verify(pub, digest[:], r, s)
}

// ParseKeySet decodes a JWKS document.
func ParseKeySet(data []byte) (JSONKeySet, error) {
	var set JSONKeySet
	if err := json.Unmarshal(data, &set); err != nil {
		return JSONKeySet{}, fmt.Errorf("jwks decode: %w", err)
	}
	if len(set.Keys) == 0 {
		return JSONKeySet{}, errors.New("jwks has no keys")
	}
	return set, nil
}
