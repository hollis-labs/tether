package api

// scoped_bindings.go — T08 (messaging vNext, CW-20260906-0039): CLI/MCP/
// client/HTTP parity for T04's scoped role/slot binding primitive
// (internal/registry/scoped_bindings.go: "a consumer-owned scope plus
// role/slot name maps to participant references," e.g. `reviewer` or
// `engineer` within a particular team/run). Before this file, T08 design
// research confirmed this primitive had ZERO external exposure of any
// kind (no HTTP route, no CLI command, no MCP tool, no internal/client
// wrapper) despite being reachable only via Go code calling
// *registry.Service directly -- this is the "scoped role resolution"
// item T08's own scope text names.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/hollis-labs/tether/internal/registry"
)

// registerScopedBindingRoutes mounts /registry/scoped-bindings. Only
// attached when Server.Registry is non-nil, matching the bindings
// routes' nil-disables-route convention.
func (s *Server) registerScopedBindingRoutes(mux *http.ServeMux) {
	if s.Registry == nil {
		return
	}
	mux.HandleFunc("/registry/scoped-bindings", s.handleScopedBindingsCollection)
	mux.HandleFunc("/registry/scoped-bindings/resolve", s.handleScopedBindingResolve)
	mux.HandleFunc("/registry/scoped-bindings/revisions", s.handleScopedBindingRevisions)
}

type scopedBindingSetRequest struct {
	Scope        string          `json:"scope"`
	Slot         string          `json:"slot"`
	TargetURNs   []string        `json:"target_urns"`
	Relationship json.RawMessage `json:"relationship,omitempty"`
	CreatedBy    string          `json:"created_by"`
}

// handleScopedBindingsCollection services POST /registry/scoped-bindings:
// publish a new revision for (scope, slot).
func (s *Server) handleScopedBindingsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed on /registry/scoped-bindings")
		return
	}
	var req scopedBindingSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	out, err := s.Registry.SetScopedBinding(r.Context(), req.Scope, req.Slot, req.TargetURNs, req.Relationship, req.CreatedBy)
	if err != nil {
		writeScopedBindingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleScopedBindingResolve services
// GET /registry/scoped-bindings/resolve?scope=&slot=[&single=true].
// single=true resolves to exactly one target URN, erroring (409) on zero
// or multiple targets rather than returning an ambiguous list for the
// caller to guess at.
func (s *Server) handleScopedBindingResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	scope, slot := q.Get("scope"), q.Get("slot")
	if scope == "" || slot == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "scope and slot are required")
		return
	}
	if q.Get("single") == "true" {
		target, binding, err := s.Registry.ResolveScopedBindingSingle(r.Context(), scope, slot)
		if err != nil {
			writeScopedBindingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"target_urn": target, "binding": binding})
		return
	}
	out, err := s.Registry.ResolveScopedBinding(r.Context(), scope, slot)
	if err != nil {
		writeScopedBindingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleScopedBindingRevisions services
// GET /registry/scoped-bindings/revisions?scope=&slot= -- the full
// provenance history, newest first.
func (s *Server) handleScopedBindingRevisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	scope, slot := q.Get("scope"), q.Get("slot")
	if scope == "" || slot == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "scope and slot are required")
		return
	}
	out, err := s.Registry.ListScopedBindingRevisions(r.Context(), scope, slot)
	if err != nil {
		writeScopedBindingError(w, err)
		return
	}
	if out == nil {
		out = []registry.ScopedBinding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": out})
}

func writeScopedBindingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, registry.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, registry.ErrAmbiguousBinding), errors.Is(err, registry.ErrBindingHasNoTargets):
		// Real, expected outcomes of a single-target resolution request
		// against a multi-target or empty-target binding -- observable,
		// not a server error.
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}
