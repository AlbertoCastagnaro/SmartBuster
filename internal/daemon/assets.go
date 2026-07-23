package daemon

import (
	"embed"
	"io/fs"
	"net/http"
)

// assetsFS is the empty scaffold spec §1/§2 call for: 5a serves an empty
// asset mount + the protocol; 5b fills this directory with the built web
// UI. Everything under /api/ is the REST+WS control plane (server.go);
// everything else falls through to these embedded static files.
//
//go:embed all:assets
var assetsFS embed.FS

// AssetHandler returns an http.Handler serving assetsFS's "assets"
// subdirectory at "/" — the same embed.FS scaffold 5b's build will replace
// wholesale, so this mount point's shape doesn't change between phases.
func AssetHandler() http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err) // assets/ is embedded at build time; this can only fail if the module itself is broken
	}
	return http.FileServer(http.FS(sub))
}
