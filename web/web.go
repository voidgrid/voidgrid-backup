// Package web holds the UI templates, embedded into the server binary.
package web

import "embed"

//go:embed templates
var FS embed.FS
