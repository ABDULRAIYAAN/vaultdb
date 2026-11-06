// Package webui holds the built playground website, embedded into the
// vaultdb-web binary so the whole site ships as one file.
//
// The source is in web/src; build it with:
//
//	cd web && npm install && npm run build
//
// The build output (web/dist) is committed, so Go users can build the
// server without Node.js.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Files returns the built site.
func Files() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
