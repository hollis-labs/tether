package api

import (
	"context"
	"net/http"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
)

// CatalogReloader re-reads the catalog from disk and swaps it in if it
// validates (CW-20261013-0002). *app.Service implements it.
type CatalogReloader interface {
	ReloadCatalog(ctx context.Context) (*config.ReloadReport, error)
}

func (s *Server) registerCatalogReloadRoutes(router *http.ServeMux) {
	if s.CatalogReload == nil {
		return
	}
	router.HandleFunc("/catalog/reload", s.handleCatalogReload)
}

// handleCatalogReload is POST /catalog/reload. A catalog that does not load or
// validate is a 409 and nothing changes; a session caller needs catalog.write.
func (s *Server) handleCatalogReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if !reloadCaller(r) {
		writeError(w, http.StatusForbidden, CodeForbidden, "reloading the catalog needs the catalog.write scope")
		return
	}
	report, err := s.CatalogReload.ReloadCatalog(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// reloadCaller reports whether the caller may reload: a session principal must
// hold catalog.write; any other verified caller, or none (local trust, as for
// the rest of the local API), may.
func reloadCaller(r *http.Request) bool {
	p, ok := identity.FromContext(r.Context())
	if !ok || p.Kind != "session" {
		return true
	}
	return identity.ScopeGranted(p.Scopes, "catalog.write")
}
