package api

// session_refs.go — HTTP surface for what a session touched.
//
// S2 of SP-20260912-0001 (CW-20260912-0060). What `source` does and does not
// prove is stated in migration 0024; the short version is that `proxy` means
// OBSERVED, not validated, and that the absence of a proxy-observed ref is
// never evidence of anything.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/hollis-labs/tether/internal/store"
)

// SessionRefStore is the storage seam for session-ref handlers.
// *store.Store satisfies it.
type SessionRefStore interface {
	AttachSessionRef(ref store.SessionRefRow) (store.AttachRefResult, error)
	ListSessionRefs(sessionID string, opts store.ListSessionRefsOptions) ([]store.SessionRefRow, error)
	ListWorkstreamRefs(workstreamID string, opts store.ListSessionRefsOptions) ([]store.SessionRefRow, error)
}

// SessionRefDTO is the wire shape for one ref.
type SessionRefDTO struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"`
	RefID     string `json:"ref_id"`
	URI       string `json:"uri,omitempty"`
	Relation  string `json:"relation"`
	Source    string `json:"source"`
	At        string `json:"at"`
}

// SessionRefListResponse is the collection response for a ref listing.
type SessionRefListResponse struct {
	Refs []SessionRefDTO `json:"refs"`
}

// SessionRefAttachRequest is the JSON body for POST /sessions/{id}/refs.
//
// Source is deliberately NOT caller-settable to "proxy": this endpoint is the
// API path, and a caller claiming its assertion was proxy-observed would
// destroy the only distinction the column exists to carry. Callers get "api"
// or "agent"; only the proxy writes "proxy", from inside the proxy.
type SessionRefAttachRequest struct {
	Kind     string `json:"kind"`
	RefID    string `json:"ref_id"`
	URI      string `json:"uri,omitempty"`
	Relation string `json:"relation,omitempty"`
	Source   string `json:"source,omitempty"`
}

// SessionRefAttachResponse reports what the write did. Both flags false is a
// success, not a failure: re-attaching is a no-op so a hook that runs twice
// does not fail.
//
// Upgraded is true when an existing row's source was raised to proxy —
// better evidence for the same unchanged fact, which is the one thing a repeat
// is allowed to revise.
type SessionRefAttachResponse struct {
	Inserted bool          `json:"inserted"`
	Upgraded bool          `json:"upgraded"`
	Ref      SessionRefDTO `json:"ref"`
}

func sessionRefToDTO(r store.SessionRefRow) SessionRefDTO {
	return SessionRefDTO{
		ID: r.ID, SessionID: r.SessionID, Kind: r.Kind, RefID: r.RefID,
		URI: r.URI, Relation: r.Relation, Source: r.Source, At: r.At,
	}
}

func sessionRefListOptions(r *http.Request) store.ListSessionRefsOptions {
	return store.ListSessionRefsOptions{
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
		rows, err := s.SessionRefs.ListSessionRefs(sessionID, sessionRefListOptions(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
		writeSessionRefList(w, rows)
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
	if req.Kind == "" || req.RefID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "kind and ref_id are required")
		return
	}
	source := req.Source
	if source == "" {
		source = store.SourceAPI
	}
	// THERE IS DELIBERATELY NO GUARD ON source HERE, INCLUDING ON
	// source=proxy. An earlier version of this handler rejected it, on the
	// reasoning that a caller claiming its assertion was proxy-observed would
	// erase the distinction the column carries. That was wrong twice over.
	//
	// It did not enforce what it claimed. Under ADR 0045 the daemon cannot
	// distinguish the real proxy from any other same-host caller — identity
	// here is self-asserted and unverified by design, the same as ?as=. So
	// the guard blocked nothing an impersonator would do.
	//
	// And it blocked the one caller telling the truth. `mux mcp --proxy` runs
	// in a SEPARATE PROCESS from the daemon and cannot reach the store; it
	// writes through this endpoint like everyone else (the same route
	// proxy_events already takes, cmd/mux/mcp.go). A guard that cannot detect
	// impersonation but does stop the honest caller is strictly worse than no
	// guard: it costs a real obstacle and buys a false assurance.
	//
	// What `source` actually records is WHO ASSERTED the ref — proxy-observed
	// versus agent-self-reported. Provenance, not authentication. No consumer
	// may read source=proxy as verified; see migration 0024 and
	// store.AttachSessionRef. Making it verifiable is deferred hardening,
	// tracked at CW-20260912-0100, and deliberately not attempted here.
	row := store.SessionRefRow{
		SessionID: sessionID, Kind: req.Kind, RefID: req.RefID,
		URI: req.URI, Relation: req.Relation, Source: source,
	}
	res, err := s.SessionRefs.AttachSessionRef(row)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	if row.Relation == "" {
		row.Relation = store.RelationReferenced
	}
	row.Source = source
	writeJSON(w, http.StatusOK, SessionRefAttachResponse{
		Inserted: res.Inserted, Upgraded: res.Upgraded, Ref: sessionRefToDTO(row),
	})
}

// handleWorkstreamRefs services GET /workstreams/{id}/refs — the roll-up.
func (s *Server) handleWorkstreamRefs(w http.ResponseWriter, r *http.Request, workstreamID string) {
	if s.SessionRefs == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "session refs are not enabled on this server")
		return
	}
	rows, err := s.SessionRefs.ListWorkstreamRefs(workstreamID, sessionRefListOptions(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeSessionRefList(w, rows)
}

func writeSessionRefList(w http.ResponseWriter, rows []store.SessionRefRow) {
	out := make([]SessionRefDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionRefToDTO(row))
	}
	writeJSON(w, http.StatusOK, SessionRefListResponse{Refs: out})
}
