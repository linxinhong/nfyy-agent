// Command eap is the Enterprise Agent Platform CLI (TASK-017/018): one
// cross-platform binary for device login, workspace/model/MCP discovery,
// skill development/sync, config inspection and environment diagnosis.
package main

import (
	"context"
	"os"

	"enterprise-agent-platform/packages/cli"
)

func main() {
	opts := cli.Options{
		// PackageKeys defaults to the platform discovery document; the
		// production binary only injects the OS keychain and streams.
		PackageKeys: cli.DefaultPackageKeys,
		Keyring:     cli.NewOSKeyring(),
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Args:        os.Args[1:],
		MinProto:    "1.0.0",
	}
	os.Exit(cli.Run(context.Background(), opts))
}
