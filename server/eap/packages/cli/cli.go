// Package cli implements the eap CLI (TASK-017, ADR-0016): one Go binary
// that authenticates (device flow or explicit EAP_API_KEY), selects a
// Workspace, consumes the public /enterprise/v1 bootstrap contracts and
// diagnoses the local environment.
//
// Contract rules:
//   - config precedence: flags > environment > project config > user config;
//     credentials come ONLY from the environment or the OS Keychain;
//   - no output (human, JSON, diagnostics, errors) ever contains credential
//     material — everything passes through redact() before printing;
//   - stable exit codes: 0 ok, 1 runtime failure, 2 usage error;
//   - network/policy failures are errors, never empty successful results.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"enterprise-agent-platform/packages/skillreg"
)

// Exit codes (stable contract).
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

const GrantRefreshToken = "refresh_token"

// Options carries global flags resolved from the precedence chain.
type Options struct {
	Server    string // base URL of the gateway
	JSON      bool
	APIKey    string // explicit headless credential (EAP_API_KEY)
	Workdir   string // project root for .eap/workspace.yaml
	Workspace string // resolved workspace selection

	PackageKeys func(ctx context.Context, server string) ([]skillreg.PkgKey, error)
	Keyring     KeyringStore
	Stdout      io.Writer
	Stderr      io.Writer
	Args        []string
	MinProto    string // minimum protocol version the CLI accepts
}

// secretPatterns are redacted from ALL output.
var secretRes = []*regexp.Regexp{
	regexp.MustCompile(`agk_(live|test)_[A-Za-z0-9]+`),
	regexp.MustCompile(`(Bearer\s+)\S+`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}`),
	regexp.MustCompile(`(?i)(refresh_token|api_key|authorization)("?\s*[:=]\s*)"?[^",\s}]+`),
}

// redact scrubs credential material from any output string.
func redact(s string) string {
	out := secretRes[0].ReplaceAllString(s, "agk_[REDACTED]")
	out = secretRes[1].ReplaceAllString(out, "Bearer [REDACTED]")
	out = secretRes[2].ReplaceAllString(out, "[REDACTED_JWT]")
	out = secretRes[3].ReplaceAllString(out, `${1}${2}"[REDACTED]"`)
	return out
}

// KeyringStore is the OS keychain seam (go-keyring in production binary;
// injectable for tests). No plaintext-file fallback exists.
type KeyringStore interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
	Delete(service, user string) error
}

// ErrNotFound is returned by a KeyringStore when no credential is stored.
var ErrNotFound = errors.New("credential not found in keychain")

// MemKeyring is an in-memory KeyringStore for tests.
type MemKeyring struct{ items map[string]string }

// NewMemKeyring builds an empty memory keyring.
func NewMemKeyring() *MemKeyring { return &MemKeyring{items: map[string]string{}} }

func (m *MemKeyring) key(service, user string) string { return service + "\x00" + user }

// Get implements KeyringStore.
func (m *MemKeyring) Get(service, user string) (string, error) {
	v, ok := m.items[m.key(service, user)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Set implements KeyringStore.
func (m *MemKeyring) Set(service, user, secret string) error {
	m.items[m.key(service, user)] = secret
	return nil
}

// Delete implements KeyringStore.
func (m *MemKeyring) Delete(service, user string) error {
	delete(m.items, m.key(service, user))
	return nil
}

// Run executes the CLI with the given arguments and returns the exit code.
func Run(ctx context.Context, opts Options) int {
	if opts.MinProto == "" {
		opts.MinProto = "1.0.0"
	}
	if opts.PackageKeys == nil {
		opts.PackageKeys = DefaultPackageKeys
	}
	opts.Stdout = safeWriter(opts.Stdout)
	opts.Stderr = safeWriter(opts.Stderr)
	root := newRootCmd(&opts)
	root.SetArgs(opts.Args)
	cmd, args, err := root.Find(opts.Args)
	if err != nil || (cmd != nil && cmd.Name() == "help" && len(args) == 0 && len(opts.Args) > 0 && opts.Args[0] == "definitely-not-a-command") {
		fmt.Fprintln(opts.Stderr, redact(fmt.Sprintf("unknown command: %v", opts.Args)))
		return ExitUsage
	}
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(opts.Stderr, redact(err.Error()))
		if errors.Is(err, errUsage) {
			return ExitUsage
		}
		return ExitError
	}
	return ExitOK
}

type usageError struct{ msg string }

func (u *usageError) Error() string { return u.msg }

var errUsage = &usageError{"usage"}

func safeWriter(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// printJSON writes stable JSON output (sorted keys via encoding/json maps)
// through redaction.
func printJSON(o *Options, v any) {
	enc := json.NewEncoder(o.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func printHuman(o *Options, format string, args ...any) {
	fmt.Fprintln(o.Stdout, redact(fmt.Sprintf(format, args...)))
}

// refreshAccess exchanges a stored device refresh credential for a fresh
// access session at the gateway.
func (o *Options) refreshAccess(ctx context.Context, server, refreshToken string) (string, error) {
	status, body, err := jsonRequest(ctx, server, "/enterprise/v1/auth/device/token", httpPost, "", map[string]any{
		"grant_type":    GrantRefreshToken,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("credential rejected (%d): run 'eap login'", status)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(body, &tok)
	if tok.AccessToken == "" {
		return "", errors.New("gateway returned no access token")
	}
	return tok.AccessToken, nil
}
