package agentdist

import (
	"os"
	"path/filepath"
)

// DefaultInstallRoot is the local adapter-owned root (~/.eap/agents).
func DefaultInstallRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".eap", "agents")
}

// PlatformDefaults returns the GOOS/GOARCH detection indirections.
var (
	// GOOSDefault is replaced in tests.
	GOOSDefault = "darwin"
	// GOARCHDefault is replaced in tests.
	GOARCHDefault = "arm64"
)

// CurrentGOOS returns the detected OS.
func CurrentGOOS() string { return GOOSDefault }

// CurrentGOARCH returns the detected architecture.
func CurrentGOARCH() string { return GOARCHDefault }
