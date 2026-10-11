// Package migrations embeds immutable SQL migrations in the server binary.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
