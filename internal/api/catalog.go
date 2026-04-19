package api

import (
	"net/http"
	"sort"

	"github.com/chrispian/agent-mux/internal/config"
)

// registerCatalogRoutes wires GET /catalog/<type> handlers onto mux.
// Catalog routes are mounted only when a CatalogLoader is configured;
// callers whose Deps.Catalog is nil see 404s (falls through to the
// daemon-level ServeMux default) rather than panicking.
//
// Reads are the full v0.0.2 catalog surface. Writes are deliberately
// not registered — see ADR 0012 and sprint v002-s08 for the scope fence.
func (s *Server) registerCatalogRoutes(mux *http.ServeMux) {
	if s.Catalog == nil {
		return
	}
	mux.HandleFunc("/catalog/projects", s.handleListProjects)
	mux.HandleFunc("/catalog/agents", s.handleListAgents)
	mux.HandleFunc("/catalog/providers", s.handleListProviders)
	mux.HandleFunc("/catalog/launches", s.handleListLaunches)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	cat, ok := s.loadCatalog(w)
	if !ok {
		return
	}
	out := make([]config.Project, 0, len(cat.Projects))
	for _, p := range cat.Projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, ListProjectsResponse{Projects: out})
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	cat, ok := s.loadCatalog(w)
	if !ok {
		return
	}
	out := make([]config.Agent, 0, len(cat.Agents))
	for _, a := range cat.Agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, ListAgentsResponse{Agents: out})
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	cat, ok := s.loadCatalog(w)
	if !ok {
		return
	}
	out := make([]config.Provider, 0, len(cat.Providers))
	for _, p := range cat.Providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, ListProvidersResponse{Providers: out})
}

func (s *Server) handleListLaunches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	cat, ok := s.loadCatalog(w)
	if !ok {
		return
	}
	out := make([]config.Launch, 0, len(cat.Launches))
	for _, l := range cat.Launches {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, ListLaunchesResponse{Launches: out})
}

// loadCatalog invokes the CatalogLoader and writes the error envelope
// on failure. Returns (cat, true) on success, (nil, false) on failure
// (with the response already written).
//
// A Load error at this seam means the catalog root is missing, the
// filesystem returned an error, or YAML parsing failed. The loader's
// own error wraps path info (e.g. "load global: read /…/global.yaml:
// no such file"), so surfacing it verbatim already cites the offending
// path. We do NOT run Validate here — dangling references (a launch
// pointing at a deleted project, say) should still render in the TUI
// with a visual warning, not blank the whole list.
func (s *Server) loadCatalog(w http.ResponseWriter) (*config.Catalog, bool) {
	cat, err := s.Catalog.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError,
			"catalog load failed: "+err.Error())
		return nil, false
	}
	if cat == nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError,
			"catalog loader returned nil")
		return nil, false
	}
	return cat, true
}
