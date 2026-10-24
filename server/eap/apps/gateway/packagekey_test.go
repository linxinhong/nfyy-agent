package main

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

type recordedKeys struct{ kids []string }

func (r *recordedKeys) RegisterPackageKey(_ context.Context, kid string, _ ed25519.PublicKey) error {
	r.kids = append(r.kids, kid)
	return nil
}

func TestPackageKeyFileIsCreatedOnceAndReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "package.key")
	reg := &recordedKeys{}
	first, err := loadPackageKey(context.Background(), path, reg)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file must be 0600: %v %v", info, err)
	}
	second, err := loadPackageKey(context.Background(), path, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.kids) != 2 || reg.kids[0] != reg.kids[1] || len(first) != 1 {
		t.Fatalf("same file, same kid: %v", reg.kids)
	}
	for kid, key := range first {
		if !key.Equal(second[kid]) {
			t.Fatal("reloading must yield the same key")
		}
	}
	if err := os.WriteFile(path, []byte("not a seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPackageKey(context.Background(), path, reg); err == nil {
		t.Fatal("a malformed key file must fail startup")
	}
}
