// Package web holds the console's server-rendered templates and static assets.
// They are embedded so a service image is a single binary with nothing to
// mount alongside it.
package web

import "embed"

// Console holds the console templates.
//
//go:embed console/*.html
var Console embed.FS
