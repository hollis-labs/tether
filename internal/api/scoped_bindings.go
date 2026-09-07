package api

// scoped_bindings.go — T08 (messaging vNext, CW-20260906-0039). HTTP
// exposure for internal/registry/scoped_bindings.go's consumer-owned
// role/slot resolution primitive (built in T04, CW-20260906-0035, but
// left Go-API-only until now). A scoped binding answers "who currently
// holds slot X in scope Y" -- e.g. scope "torque:sprint:CW-20260906-0023"
// slot "reviewer" resolves to one or more registry URNs. Pure resolution/
// mapping, no workflow/staffing/activation semantics (those remain
// consumer-owned, per the architecture doc).
//
// Trust model: same as the rest of /registry/* and /messages/* (ADR
// 0045) -- created_by is a self-asserted provenance label, not a
// verified identity.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/hollis-labs/tether/internal/registry"
)

type setScopedBindingRequest struct {
	Scope        string          `json:"scope"`
	Slot         string          `json:"slot"`
	TargetURNs   []string        `json:"target_urns"`
	Relationship json.RawMessage `json:"relationship,omitempty"`
	CreatedBy    string          `json:"created_by"`
}

// registerScopedBindingRoutes mounts /registry/scoped-bindings. No-op
// when Registry is nil (matches every other optional dependency in this
// package).
func (s *Server) registerScopedBindingRoutes(mux *http.ServeMux) {
	if s.Registry == nil {
		return
	}
	mux.HandleFunc("/registry/scoped-bindings", s.handleScopedBindingsCollection)
	mux.HandleFunc("/registry/scoped-bindings/history", s.handleScopedBindingHistory)
}

// handleScopedBindingsCollection services /registry/scoped-bindings:
// POST publishes a new revision; GET resolves the current one.
func (s *Server) handleScopedBindingsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleSetScopedBinding(w, r)
	case http.MethodGet:
		s.handleResolveScopedBinding(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed on /registry/scoped-bindings")
	}
}

func (s *Server) handleSetScopedBinding(w http.ResponseWriter, r *http.Request) {
	var req setScopedBindingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	b, err := s.Registry.SetScopedBinding(r.Context(), req.Scope, req.Slot, req.TargetURNs, req.Relationship, req.CreatedBy)
	if err != nil {
		writeScopedBindingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

// handleResolveScopedBinding services GET /registry/scoped-bindings
// ?scope=&slot=[&single=true]. single=true uses ResolveScopedBindingSingle,
// erroring (409) on zero or multiple current targets instead of returning
// the full target list -- for a caller that needs exactly one recipient
// and wants an explicit, observable ambiguity error rather than picking
// one itself.
func (s *Server) handleResolveScopedBinding(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	slot := r.URL.Query().Get("slot")
	if scope == "" || slot == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "scope and slot query params are required")
		return
	}
	if r.URL.Query().Get("single") == "true" {
		target, b, err := s.Registry.ResolveScopedBindingSingle(r.Context(), scope, slot)
		if err != nil {
			writeScopedBindingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"target_urn": target, "binding": b})
		return
	}
	b, err := s.Registry.ResolveScopedBinding(r.Context(), scope, slot)
	if err != nil {
		writeScopedBindingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// handleScopedBindingHistory services GET /registry/scoped-bindings/history
// ?scope=&slot= -- the full provenance history, newest revision first.
func (s *Server) handleScopedBindingHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	scope := r.URL.Query().Get("scope")
	slot := r.URL.Query().Get("slot")
	if scope == "" || slot == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "scope and slot query params are required")
		return
	}
	rows, err := s.Registry.ListScopedBindingRevisions(r.Context(), scope, slot)
	if err != nil {
		writeScopedBindingError(w, err)
		return
	}
	if rows == nil {
		rows = []registry.ScopedBinding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": rows})
}

func writeScopedBindingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, registry.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, registry.ErrBindingHasNoTargets):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, registry.ErrAmbiguousBinding):
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}
