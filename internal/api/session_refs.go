package api

// session_refs.go — HTTP surface for what a session touched.
//
// Source is derived from verified caller identity (CW-20261016-0157).
// Historical proxy_claimed rows are untrusted; no source validates an object.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/hollis-labs/tether/internal/app/sessionrefs"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// SessionRefStore is the narrow composition seam for session reference storage.
type SessionRefStore = sessionrefs.RecordStore

// SessionRefDTO is the application-owned wire contract, explicitly translated from storage.
type SessionRefDTO = sessionrefs.SessionRefDTO
type SessionRefListResponse = sessionrefs.SessionRefListResponse
type SessionRefAttachRequest sessionrefs.SessionRefAttachRequest
type SessionRefAttachResponse = sessionrefs.SessionRefAttachResponse

func sessionRefListOptions(r *http.Request) sessionrefs.Filters {
	return sessionrefs.Filters{
		Kind:     r.URL.Query().Get("kind"),
		Relation: r.URL.Query().Get("relation"),
		Source:   r.URL.Query().Get("source"),
	}
}

// handleSessionRefs services POST and GET /sessions/{id}/refs.
func (s *Server) handleSessionRefs(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.SessionRefs == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "session refs are not enabled on this server")
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handleAttachSessionRef(w, r, sessionID)
	case http.MethodGet:
		out, err := sessionrefs.NewRecords(s.SessionRefs).ListSession(sessionID, sessionRefListOptions(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAttachSessionRef(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req SessionRefAttachRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid JSON body: "+err.Error())
		return
	}
	// CW-20261016-0157: no request field can confer observed provenance.
	// Only the hub-owned MCP capture path writes SourceProxy internally.
	// No HTTP principal derives it, including an operator or service named proxy.
	if req.Source == store.SourceProxy {
		if s.refSourceAssertions != nil {
			s.refSourceAssertions.Add(r.Context(), 1)
		}
		writeError(w, http.StatusForbidden, "source_asserted", "proxy source is reserved for hub-observed traffic")
		return
	}
	req.Source = store.SourceAPI // legacy anonymous/device/service HTTP capture
	if p, ok := identity.FromContext(r.Context()); ok && p.Verified {
		switch {
		case identity.IsOperatorCredential(p):
			req.Source = store.SourceOperator
		case p.Kind == "session" || p.Kind == "agent" && p.SessionID != "":
			req.Source = store.SourceSession
		}
	}
	out, err := sessionrefs.NewRecords(s.SessionRefs).Attach(sessionID, sessionrefs.SessionRefAttachRequest(req))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWorkstreamRefs services GET /workstreams/{id}/refs — the roll-up.
func (s *Server) handleWorkstreamRefs(w http.ResponseWriter, r *http.Request, workstreamID string) {
	if s.SessionRefs == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "session refs are not enabled on this server")
		return
	}
	out, err := sessionrefs.NewRecords(s.SessionRefs).ListWorkstream(workstreamID, sessionRefListOptions(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
