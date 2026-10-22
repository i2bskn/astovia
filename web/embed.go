// Package web provides the built management interface.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var content embed.FS

// Assets returns paths relative to the Vite output directory.
func Assets() (fs.FS, error) {
	return fs.Sub(content, "dist")
}
