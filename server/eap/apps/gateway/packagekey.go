package main

// Release-signing key for the Skill Registry.
//
// Why a file and not the database: package signatures must stay verifiable
// offline and the private key must never be readable through the control
// store (unlike the JWT keys), so it lives in an operator-managed file. The
// gateway creates it on first start when the path does not exist yet, so a
// single-node install works without a separate key ceremony; operators who
// manage keys elsewhere drop their own seed there.
//
// Pitfalls: the file holds the raw 32-byte Ed25519 seed, base64url-encoded,
// written 0600. Every gateway instance must point at the same file content,
// or releases flip-flop the active key; the kid derives from the public key,
// so equal files register idempotently.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type packageKeyRegistrar interface {
	RegisterPackageKey(ctx context.Context, kid string, pub ed25519.PublicKey) error
}

// loadPackageKey reads (or first creates) the seed at path and registers its
// public key as the active release key.
func loadPackageKey(ctx context.Context, path string, reg packageKeyRegistrar) (map[string]ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, fmt.Errorf("generate package key: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("package key dir: %w", err)
		}
		raw = []byte(base64.RawURLEncoding.EncodeToString(seed) + "\n")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create package key: %w", err)
		}
		if _, err := f.Write(raw); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("write package key: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("write package key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read package key: %w", err)
	}
	seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("package key file must hold a base64url 32-byte Ed25519 seed")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	kid := "pkg-" + hex.EncodeToString(sum[:8])
	if err := reg.RegisterPackageKey(ctx, kid, pub); err != nil {
		return nil, err
	}
	return map[string]ed25519.PrivateKey{kid: priv}, nil
}
