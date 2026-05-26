package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/hollis-labs/agentkit/agentsessions"
)

// maxInputBytes caps the per-request body size for POST /sessions/{id}/input
// so a misbehaving client can't exhaust daemon memory writing into a PTY.
// 1 MiB per call is comfortably larger than a paste buffer; streaming input
// is not a v0.0.2 concern.
const maxInputBytes = 1 << 20

// handleSendInput reads raw bytes from the request body and writes them to
// the named session's PTY. The body is treated as an opaque byte stream:
// no framing, no newline normalisation. The CLI is free to append a
// trailing newline for the text-arg convenience; API callers own their
// own framing.
func (s *Server) handleSendInput(w http.ResponseWriter, r *http.Request, id string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInputBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "read body: "+err.Error())
		return
	}
	if len(body) > maxInputBytes {
		writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "input too large")
		return
	}
	if err := s.Service.SendInput(id, body); err != nil {
		if errors.Is(err, agentsessions.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not running")
			return
		}
		if errors.Is(err, agentsessions.ErrNoInputChannel) {
			writeError(w, http.StatusConflict, CodeConflict, "session has no input channel")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SendTurnRequest is the body of POST /sessions/{id}/turn. text is the
// user-facing message; the daemon applies per-runtime framing (NDJSON
// envelope for streaming-stdio, JSON-RPC turn/start for jsonrpc-stdio,
// raw write for PTY or unknown modes).
type SendTurnRequest struct {
	Text string `json:"text"`
}

// handleSendTurn delivers a user message with lifecycle-aware framing.
// Unlike /input (raw bytes), /turn accepts a string and lets the
// service.SendTurn dispatcher apply the right wire shape based on the
// session's caps. Useful for callers that don't want to reimplement
// NDJSON envelope encoding or codex JSON-RPC turn semantics.
func (s *Server) handleSendTurn(w http.ResponseWriter, r *http.Request, id string) {
	var req SendTurnRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxInputBytes+1)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "decode body: "+err.Error())
		return
	}
	if err := s.Service.SendTurn(r.Context(), id, req.Text); err != nil {
		if errors.Is(err, agentsessions.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not running")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
