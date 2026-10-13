package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/hollis-labs/tether/internal/store"
)

// ForceStopOptions selects the force-close path of POST /sessions/{id}/stop.
type ForceStopOptions struct {
	// Reason is recorded on the session.force_closed event.
	Reason string
	// AllowLive lets the close proceed when a live worker or agent still
	// holds the session. Without it a live session is refused.
	AllowLive bool
}

// ForceStopResult reports a force-closed session row.
type ForceStopResult struct {
	SessionID string `json:"session_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Reason    string `json:"reason"`
	// WasLive is true when AllowLive overrode a live worker or agent.
	WasLive bool `json:"was_live"`
}

// ErrSessionLive is returned by a force close when a live worker or agent
// still holds the session and AllowLive was not set.
var ErrSessionLive = errors.New("session is live; use a plain stop, or allow_live to force-close it")

// ErrSessionAlreadyTerminal is returned when the row is already terminal.
var ErrSessionAlreadyTerminal = errors.New("session is already in a terminal state")

// ForceStopService is the optional seam behind POST /sessions/{id}/stop?force=true.
// A Service that does not implement it answers 501.
type ForceStopService interface {
	ForceStopSession(ctx context.Context, id string, opts ForceStopOptions) (ForceStopResult, error)
}

func queryFlag(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func (s *Server) handleForceStopSession(w http.ResponseWriter, r *http.Request, id string) {
	svc, ok := s.Service.(ForceStopService)
	if !ok {
		writeError(w, http.StatusNotImplemented, CodeNotImplemented, "force stop is not supported by this daemon")
		return
	}
	res, err := svc.ForceStopSession(r.Context(), id, ForceStopOptions{
		Reason:    strings.TrimSpace(r.URL.Query().Get("reason")),
		AllowLive: queryFlag(r, "allow_live"),
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, store.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "session not found")
	case errors.Is(err, ErrSessionLive):
		writeError(w, http.StatusConflict, "session_live", err.Error())
	case errors.Is(err, ErrSessionAlreadyTerminal):
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}
