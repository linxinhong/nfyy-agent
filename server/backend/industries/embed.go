// Package industries embeds the default industry packs in the Go binary. A
// private deployment may override them with YUANPU_INDUSTRY_ROOT.
package industries

import "embed"

//go:embed legal
var Files embed.FS
