//go:build !windows

package cli

// OS keychain integration isolated here so the storage seam stays swappable
// and testable. Production binary: macOS `security` / Linux `secret-tool`.
// No plaintext-file fallback exists by contract.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// newOSKeyring returns the platform keyring store.
func NewOSKeyring() KeyringStore { return &osKeyring{} }

type osKeyring struct{}

func (k *osKeyring) Get(service, user string) (string, error) {
	if isDarwin() {
		out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", user, "-w").Output()
		if err != nil {
			return "", ErrNotFound
		}
		return strings.TrimSpace(string(out)), nil
	}
	out, err := exec.Command("secret-tool", "lookup", "service", service, "user", user).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", ErrNotFound
	}
	return strings.TrimSpace(string(out)), nil
}

func (k *osKeyring) Set(service, user, secret string) error {
	if isDarwin() {
		_ = exec.Command("security", "delete-generic-password", "-s", service, "-a", user).Run()
		out, err := exec.Command("security", "add-generic-password", "-s", service, "-a", user, "-U", "-w", secret).CombinedOutput()
		if err != nil {
			return fmt.Errorf("keychain write failed: %v: %s", err, redact(string(out)))
		}
		return nil
	}
	cmd := exec.Command("secret-tool", "store", "--label=eap-cli", "service", service, "user", user)
	cmd.Stdin = strings.NewReader(secret)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain write failed: %v: %s", err, redact(string(out)))
	}
	return nil
}

func (k *osKeyring) Delete(service, user string) error {
	if isDarwin() {
		return exec.Command("security", "delete-generic-password", "-s", service, "-a", user).Run()
	}
	return exec.Command("secret-tool", "clear", "service", service, "user", user).Run()
}

var osGetenv = os.Getenv

func isDarwin() bool { return runtimeGOOS == "darwin" }
