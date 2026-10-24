//go:build !windows

package cli

import "runtime"

const runtimeGOOS = runtime.GOOS
