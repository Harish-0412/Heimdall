// Package migrations embeds the goose SQL migrations in control-plane binaries.
package migrations

import "embed"

// Files contains the versioned control-plane schema.
//
//go:embed *.sql
var Files embed.FS
