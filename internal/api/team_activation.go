package api

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"io"
	"net/http"
	"net/url"

	"github.com/hollis-labs/tether/internal/teamactivation"
)

type teamActivationInspector interface {
	InspectTeamActivation(context.Context, teamactivation.Query) (teamactivation.Evidence, error)
}

func (s *Server) registerTeamActivationRoute(mux *http.ServeMux) {
	ops, ok := s.Service.(teamActivationInspector)
	if !ok {
		return
	}
	mux.HandleFunc("/teams/activation", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
			return
		}
		// Match the existing catalog planning authority; this evidence includes
		// effective permission/placement facts, never a new admission grant.
		if !catalogWriter(r) {
			writeError(w, 403, CodeForbidden, "inspection needs catalog.write scope")
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeError(w, 400, CodeInvalidRequest, "invalid query")
			return
		}
		for key, v := range values {
			if len(v) != 1 || key != "launch_id" && key != "session_id" {
				writeError(w, 400, CodeInvalidRequest, "invalid selector")
				return
			}
		}
		q := teamactivation.Query{LaunchID: values.Get("launch_id"), SessionID: values.Get("session_id")}
		if q.LaunchID == "" {
			writeError(w, 400, CodeInvalidRequest, "launch_id required")
			return
		}
		out, err := ops.InspectTeamActivation(r.Context(), q)
		if err != nil {
			writeError(w, 409, CodeConflict, "activation evidence unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, out)
	})
}

// Targeted proposals retain catalogWriter and caller-definition protection.
func (s *Server) handleTeamActivationPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if !catalogWriter(r) {
		writeError(w, 403, CodeForbidden, "planning needs catalog.write scope")
		return
	}
	ops, ok := s.CatalogApply.(interface {
		PlanTeamActivation(context.Context, catalogapply.TeamActivationRequest, catalogapply.Options) (*catalogapply.Plan, error)
	})
	if !ok {
		writeError(w, 404, CodeNotFound, "activation planning unavailable")
		return
	}
	var req catalogapply.TeamActivationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, 400, CodeInvalidRequest, "invalid request")
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		writeError(w, 400, CodeInvalidRequest, "exactly one request required")
		return
	}
	plan, err := ops.PlanTeamActivation(r.Context(), req, catalogapply.Options{ProtectedPaths: s.callerDefinitionPaths(r), Caller: s.callerOf(r)})
	if err != nil {
		writeError(w, 400, CodeInvalidRequest, err.Error())
		return
	}
	writeJSON(w, 200, plan)
}
