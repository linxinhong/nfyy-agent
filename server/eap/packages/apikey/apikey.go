// Package apikey implements the employee API key format and hashing contract
// (PRD 6.3): keys look like agk_live_<random> / agk_test_<random>, are shown
// in full exactly once at creation, and are stored only as SHA-256 hashes.
// SHA-256 is sufficient here because the keys are 128+ bit random values, not
// user-chosen secrets; it also enables O(1) lookup by hash on every request.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Environment prefixes.
const (
	EnvLive = "live"
	EnvTest = "test"
)

// randomLength yields 192 bits of entropy in base62 (~32 chars).
const randomLength = 32

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Public prefix that identifies a key on the wire.
const PublicPrefix = "agk_"

// Errors for malformed credentials.
var (
	ErrEmpty     = errors.New("api key is empty")
	ErrBadFormat = errors.New("api key format is invalid")
)

// Generated is one newly minted key. Full is the one-time display value;
// only Hash may be persisted.
type Generated struct {
	Full   string // agk_live_xxx — one-time display
	Prefix string // agk_live_<8 chars> — safe display
	Hash   string // sha256 hex of Full — the only stored form
}

// Generate mints a key for the given environment ("live" or "test").
func Generate(environment string) (Generated, error) {
	if environment != EnvLive && environment != EnvTest {
		return Generated{}, fmt.Errorf("api key environment must be %s or %s", EnvLive, EnvTest)
	}
	b := make([]byte, randomLength)
	if _, err := rand.Read(b); err != nil {
		return Generated{}, fmt.Errorf("crypto/rand unavailable: %w", err)
	}
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(base62Alphabet[int(c)%len(base62Alphabet)])
	}
	full := PublicPrefix + environment + "_" + sb.String()
	return Generated{Full: full, Prefix: DisplayPrefix(full), Hash: Hash(full)}, nil
}

// Hash returns the hex sha256 of the full key value — the only form stored.
func Hash(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

// DisplayPrefix returns the safe-to-show prefix (first 13 chars):
// agk_live_abcdefgh.
func DisplayPrefix(full string) string {
	if len(full) < 13 {
		return full
	}
	return full[:13]
}

// ValidateFormat checks the wire shape without touching storage. It reports
// ErrBadFormat for anything that is not agk_<env>_<random>.
func ValidateFormat(full string) error {
	if full == "" {
		return ErrEmpty
	}
	if !strings.HasPrefix(full, PublicPrefix) {
		return ErrBadFormat
	}
	rest := strings.TrimPrefix(full, PublicPrefix)
	env, _, found := strings.Cut(rest, "_")
	if !found || (env != EnvLive && env != EnvTest) {
		return ErrBadFormat
	}
	secret := strings.TrimPrefix(rest, env+"_")
	if len(secret) < 16 {
		return ErrBadFormat
	}
	return nil
}
