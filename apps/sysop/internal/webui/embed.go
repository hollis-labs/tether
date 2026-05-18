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
// with `base` in frontend/vite.config.ts.
const BasePath = "/operations"

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

// Mount registers the Sysop UI handler on mux at BasePath. Both the
// trailing-slash and bare patterns are registered so /operations
// and /operations/ both resolve.
func Mount(mux *http.ServeMux) {
	h := Handler()
	mux.Handle(BasePath+"/", h)
	mux.Handle(BasePath, h)
}
