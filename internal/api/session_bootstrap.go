package api

// session_bootstrap.go — T08 (messaging vNext, CW-20260906-0039): the
// "provider-neutral local bootstrap/registration helper" endpoint the
// architecture calls for: "agent-setup can invoke at the launch/host
// boundary; accept preassigned SESSION and idempotent fallback... A
// second hook invocation must not invent a competing identity."
//
// Before this file, T02 built the canonical-session-identity vocabulary
// (SessionRow.Intent/ParentSessionID/Publication, session_provider_mappings)
// but every writer of it lived entirely inside Tether's own launch/resume
// code path (internal/app/session_lifecycle.go, resume.go) -- there was
// no way for an EXTERNAL process (a launcher Tether didn't itself start)
// to register a session's canonical identity. This endpoint is that seam.
//
// Idempotency: a session_id that already has a row is a no-op on the row
// itself (created=false in the response) -- this is what makes a repeated
// hook call for the same preassigned SESSION safe rather than inventing a
// competing identity. Provider mappings are always applied regardless of
// created/existing, since UpsertSessionProviderMapping is itself an
// upsert keyed on (session_id, owner, provider) -- this is what makes a
// later "reconnect" call (same session_id, a freshly-observed provider
// mapping) work without erroring.
//
// This endpoint does NOT launch, resume, or otherwise take lifecycle
// authority over anything -- it only records identity. A session
// bootstrapped this way gets state="external": distinct from any state
// Tether's own launch/resume flow would set, so nothing downstream
// mistakes it for a process Tether is managing.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

// SessionBootstrapStore is the narrow seam POST /sessions/bootstrap
// depends on. *store.Store satisfies it directly.
type SessionBootstrapStore interface {
	GetSession(id string) (*store.SessionRow, error)
	CreateSession(row store.SessionRow, plan *launch.Plan) error
	UpsertSessionProviderMapping(sessionID, owner, provider, nativeSessionID string) error
}

type sessionBootstrapProviderMapping struct {
	Owner           string `json:"owner"`
	Provider        string `json:"provider"`
	NativeSessionID string `json:"native_session_id"`
}

type sessionBootstrapRequest struct {
	// SessionID is the canonical SESSION value the caller has already
	// resolved (preassigned via env var, or freshly minted client-side)
	// -- this endpoint never mints one itself; minting without an online
	// Tether is exactly the property go-tether-client's bootstrap helper
	// provides.
	SessionID        string                            `json:"session_id"`
	Intent           string                            `json:"intent"` // preassigned|resume|compact|fresh|fork; default "preassigned"
	ParentSessionID  string                            `json:"parent_session_id,omitempty"`
	LogicalAgentID   string                            `json:"logical_agent_id,omitempty"` // set for a durable-actor bootstrap
	Publication      string                            `json:"publication,omitempty"`      // private-local|published-local|tether-hosted; default "private-local"
	ProviderMappings []sessionBootstrapProviderMapping `json:"provider_mappings,omitempty"`
}

type sessionBootstrapResponse struct {
	SessionID string `json:"session_id"`
	// Created is false when session_id already had a row -- the
	// idempotent/repeated-call/reconnect case.
	Created bool `json:"created"`
}

// registerSessionBootstrapRoutes mounts POST /sessions/bootstrap. Only
// attached when Server.SessionBootstrap is non-nil, matching every other
// optional dependency's nil-disables-route convention.
func (s *Server) registerSessionBootstrapRoutes(mux *http.ServeMux) {
	if s.SessionBootstrap == nil {
		return
	}
	mux.HandleFunc("/sessions/bootstrap", s.handleSessionBootstrap)
}

func (s *Server) handleSessionBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	var req sessionBootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "session_id is required")
		return
	}

	created := false
	if _, err := s.SessionBootstrap.GetSession(req.SessionID); err != nil {
		if !errors.Is(err, store.ErrSessionNotFound) {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
		intent := req.Intent
		if intent == "" {
			intent = "preassigned"
		}
		row := store.SessionRow{
			ID:             req.SessionID,
			LogicalAgentID: req.LogicalAgentID,
			State:          "external",
			Intent:         intent,
			Publication:    req.Publication,
		}
		if req.ParentSessionID != "" {
			row.ParentSessionID = sql.NullString{String: req.ParentSessionID, Valid: true}
		}
		if err := s.SessionBootstrap.CreateSession(row, nil); err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
		created = true
	}

	for _, m := range req.ProviderMappings {
		if m.Owner == "" || m.Provider == "" || m.NativeSessionID == "" {
			continue // best-effort: an incomplete entry doesn't fail the whole call
		}
		if err := s.SessionBootstrap.UpsertSessionProviderMapping(req.SessionID, m.Owner, m.Provider, m.NativeSessionID); err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, sessionBootstrapResponse{SessionID: req.SessionID, Created: created})
}
