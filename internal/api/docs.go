package api

import (
	"net/http"

	docs "github.com/hollis-labs/tether"
)

// DocsService is the read-only service shared with the MCP adapter.
type DocsService interface {
	List() []docs.DocMetadata
	Get(string) (docs.DocBody, error)
	GetFile(string, string) (docs.DocFileBody, error)
}

func (s *Server) registerDocsRoutes(router *http.ServeMux) {
	if s.Docs == nil {
		return
	}
	get := func(pattern string, handler http.HandlerFunc) {
		router.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
				return
			}
			handler(w, r)
		})
	}
	get("/docs/mcp", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Docs.List()) })
	get("/docs/mcp/{id}", func(w http.ResponseWriter, r *http.Request) {
		doc, err := s.Docs.Get(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, doc)
	})
	get("/docs/mcp/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		file, err := s.Docs.GetFile(r.PathValue("id"), r.URL.Query().Get("path"))
		if err != nil {
			writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, file)
	})
}
