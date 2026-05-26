// Package webui embeds the built Sysop UI frontend and serves it through
// the shared go-webui harness.
package webui

import (
	"embed"
	"io/fs"
	"net/http"

	gowebui "github.com/hollis-labs/go-webui"
)

// BasePath is the URL prefix the Sysop UI is mounted at. Keep it in sync
// with `base` in frontend/vite.config.ts. An empty string mounts at the
// site root; non-API paths (e.g. /operations, /overview) are then treated
// as client-side routes by the SPA.
const BasePath = ""

// embedded holds the frontend build. `make ui-build` (vite) writes the
// real assets into dist/; until then dist/ holds only a placeholder and
// go-webui serves its "not built" page.
//
//go:embed all:dist
var embedded embed.FS

// Handler returns the http.Handler serving the embedded Sysop UI.
func Handler() http.Handler {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic("webui: embedded dist directory missing: " + err.Error())
	}
	return gowebui.Handler(gowebui.Config{FS: dist, BasePath: BasePath})
}

// Mount registers the Sysop UI handler on mux. When BasePath is empty the
// handler is mounted at the site root; more specific patterns (e.g.
// /api/*) registered on the same mux still take precedence.
func Mount(mux *http.ServeMux) {
	h := Handler()
	if BasePath == "" {
		mux.Handle("/", h)
		return
	}
	mux.Handle(BasePath+"/", h)
	mux.Handle(BasePath, h)
}
