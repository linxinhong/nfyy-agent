package apikey_test

import (
	"errors"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/apikey"
)

func TestGenerateShape(t *testing.T) {
	g, err := apikey.Generate(apikey.EnvLive)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(g.Full, "agk_live_") {
		t.Fatalf("live key prefix wrong: %s", g.Full)
	}
	if g.Prefix != g.Full[:13] {
		t.Fatalf("display prefix must be the first 13 chars: %q", g.Prefix)
	}
	if g.Hash != apikey.Hash(g.Full) {
		t.Fatalf("hash mismatch")
	}
	if len(g.Hash) != 64 {
		t.Fatalf("sha256 hex must be 64 chars")
	}
	if g.Full == g.Hash || strings.Contains(g.Hash, "agk_") {
		t.Fatalf("hash must not embed the key material")
	}
	if err := apikey.ValidateFormat(g.Full); err != nil {
		t.Fatalf("generated key must pass validation: %v", err)
	}
}

func TestGenerateTestEnvironment(t *testing.T) {
	g, err := apikey.Generate(apikey.EnvTest)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(g.Full, "agk_test_") {
		t.Fatalf("test key prefix wrong: %s", g.Full)
	}
}

func TestGenerateRejectsUnknownEnvironment(t *testing.T) {
	if _, err := apikey.Generate("staging"); err == nil {
		t.Fatalf("unknown environment must be rejected")
	}
}

// One-time display: the full value can only be reconstructed by regenerating;
// stored data (prefix + hash) must never reveal it.
func TestStoredDataDoesNotLeakFullKey(t *testing.T) {
	g, err := apikey.Generate(apikey.EnvLive)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	stored := g.Prefix + g.Hash
	if strings.Contains(stored, g.Full) {
		t.Fatalf("stored metadata must not contain the full key")
	}
}

func TestHashDeterministic(t *testing.T) {
	full := "agk_live_0123456789abcdefghijklmnopqrstuvwxyzAB"
	if apikey.Hash(full) != apikey.Hash(full) {
		t.Fatalf("hash must be deterministic")
	}
	if apikey.Hash(full) == apikey.Hash(full+"x") {
		t.Fatalf("different keys must hash differently")
	}
}

func TestValidateFormat(t *testing.T) {
	good := "agk_live_0123456789abcdef"
	if err := apikey.ValidateFormat(good); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	cases := map[string]error{
		"":                apikey.ErrEmpty,
		"agk_live_short":  apikey.ErrBadFormat,
		"gk_live_shortxx": apikey.ErrBadFormat,
		"agk_stag_xxxxxx": apikey.ErrBadFormat,
		"agk_live_":       apikey.ErrBadFormat,
		"not-a-key":       apikey.ErrBadFormat,
	}
	for input, want := range cases {
		err := apikey.ValidateFormat(input)
		if !errors.Is(err, want) {
			t.Errorf("ValidateFormat(%q) = %v, want %v", input, err, want)
		}
	}
}

func TestUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		g, err := apikey.Generate(apikey.EnvLive)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if seen[g.Full] {
			t.Fatalf("duplicate key generated")
		}
		seen[g.Full] = true
	}
}
