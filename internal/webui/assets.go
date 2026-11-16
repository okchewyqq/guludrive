// Package webui contains the Web UI built by Vite. Run the web build target
// before compiling Go from a fresh checkout. Only dist is embedded, never the
// repository-level distribution directory or writable application data.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var bundled embed.FS

// Assets returns the read-only Web assets rooted at index.html.
func Assets() fs.FS {
	assets, err := fs.Sub(bundled, "dist")
	if err != nil {
		// The constant above is a valid fs path; this cannot fail at runtime.
		panic("webui: invalid embedded asset root: " + err.Error())
	}
	return assets
}
