package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// DistAssets embeds the production single-page application bundle.
// If empty (e.g. in test environments), the server provides a helpful fallback.
//
//go:embed all:dist
var DistAssets embed.FS

// StaticFileSystem returns an http.FileSystem rooted at the embedded dist directory,
// stripping the "dist" prefix so that index.html is served at the root.
func StaticFileSystem() http.FileSystem {
	sub, err := fs.Sub(DistAssets, "dist")
	if err != nil {
		return http.FS(DistAssets)
	}
	return http.FS(sub)
}
