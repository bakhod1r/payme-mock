// Package migrations embeds the schema so every service can bring the
// database up to date on its own, without a separate migration image. The
// .sql files beside this one are the single source of truth for the schema.
package migrations

import "embed"

// FS holds the goose migration files.
//
//go:embed *.sql
var FS embed.FS
