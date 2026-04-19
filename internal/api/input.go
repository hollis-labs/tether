package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/runtime"
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
		if errors.Is(err, runtime.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not running")
			return
		}
		if errors.Is(err, provider.ErrNoInputChannel) {
			writeError(w, http.StatusConflict, CodeConflict, "session has no input channel")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
