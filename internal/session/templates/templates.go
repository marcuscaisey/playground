// Package templates provides the built-in templates.
package templates

import "embed"

// FS is a file system containing the built-in templates.
//
//go:embed *
var FS embed.FS
