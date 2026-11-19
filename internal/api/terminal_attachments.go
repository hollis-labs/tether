package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/hollis-labs/tether/internal/terminalattach"
)

// /terminal-attachments is an opt-in view/control surface. Host-owned PTY
// bytes are still served by the existing attach transport; this surface adds
// exact-binding checks and explicit writer arbitration for UI attachments.
func (s *Server) registerTerminalAttachmentRoutes(router *http.ServeMux) {
	if s.Terminal == nil {
		return
	}
	router.HandleFunc("/terminal-attachments", s.terminalAttachmentCollection)
	router.HandleFunc("/terminal-attachments/", s.terminalAttachmentItem)
}

func (s *Server) terminalAttachmentCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		Target string              `json:"target"`
		Role   terminalattach.Role `json:"role"`
	}
	if err := decodeTerminalJSON(r, &in); err != nil || in.Target == "" {
		writeError(w, 400, CodeInvalidRequest, "target required")
		return
	}
	a, err := s.Terminal.Open(r.Context(), in.Target, in.Role)
	if err != nil {
		terminalError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) terminalAttachmentItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/terminal-attachments/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		writeError(w, 404, CodeNotFound, "attachment not found")
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodDelete {
			writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
			return
		}
		if err := s.Terminal.Close(r.Context(), id); err != nil {
			terminalError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	lease := r.Header.Get("X-Tether-Writer-Lease")
	switch parts[1] {
	case "lease":
		if r.Method != http.MethodPost {
			writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
			return
		}
		var in struct {
			Takeover bool `json:"takeover"`
		}
		if err := decodeTerminalJSON(r, &in); err != nil {
			writeError(w, 400, CodeInvalidRequest, "invalid lease request")
			return
		}
		a, token, err := s.Terminal.Acquire(r.Context(), id, in.Takeover)
		if err != nil {
			terminalError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, struct {
			Attachment terminalattach.Attachment `json:"attachment"`
			Lease      string                    `json:"lease"`
		}{a, token})
	case "renew":
		if r.Method != http.MethodPost {
			writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
			return
		}
		a, err := s.Terminal.Renew(r.Context(), id, lease)
		if err != nil {
			terminalError(w, err)
			return
		}
		writeJSON(w, 200, a)
	case "input":
		if r.Method != http.MethodPost {
			writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxInputBytes+1))
		if err != nil {
			writeError(w, 400, CodeInvalidRequest, "cannot read input")
			return
		}
		if len(data) > maxInputBytes {
			writeError(w, 413, CodePayloadTooLarge, "input too large")
			return
		}
		if err := s.Terminal.Input(r.Context(), id, lease, data); err != nil {
			terminalError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "resize":
		if r.Method != http.MethodPost {
			writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
			return
		}
		var in struct {
			Rows uint16 `json:"rows"`
			Cols uint16 `json:"cols"`
		}
		if err := decodeTerminalJSON(r, &in); err != nil || in.Rows == 0 || in.Cols == 0 {
			writeError(w, 400, CodeInvalidRequest, "rows and cols must be > 0")
			return
		}
		if err := s.Terminal.Resize(r.Context(), id, lease, in.Rows, in.Cols); err != nil {
			terminalError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "stream":
		if r.Method != http.MethodGet {
			writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
			return
		}
		since := int64(0)
		if raw := r.URL.Query().Get("since_offset"); raw != "" {
			var err error
			since, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || since < 0 {
				writeError(w, 400, CodeInvalidRequest, "since_offset must be a non-negative byte offset")
				return
			}
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, 500, CodeInternalError, "streaming not supported")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		started := false
		err := s.Terminal.Observe(r.Context(), id, &flushWriter{w: w, f: flusher}, since, func(window terminalattach.Window, gap bool) error {
			w.Header().Set("X-Tether-Attach-Oldest-Offset", strconv.FormatInt(window.OldestOffset, 10))
			w.Header().Set("X-Tether-Attach-Next-Offset", strconv.FormatInt(window.NextOffset, 10))
			w.Header().Set("X-Tether-Attach-Evicted", strconv.FormatBool(gap))
			w.Header().Set("X-Tether-Attach-Cursor", "byte-offset")
			w.Header().Set("X-Tether-Terminal-Diagnostic", "true")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			started = true
			return nil
		})
		if err != nil && !started {
			terminalError(w, err)
		}
	default:
		writeError(w, 404, CodeNotFound, "terminal attachment action not found")
	}
}

func decodeTerminalJSON(r *http.Request, out any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil {
		return err
	}
	if len(data) > 4096 {
		return errors.New("request too large")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func terminalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, terminalattach.ErrNotFound):
		writeError(w, 404, CodeNotFound, err.Error())
	case errors.Is(err, terminalattach.ErrNotTerminal):
		writeError(w, 409, CodeConflict, err.Error())
	case errors.Is(err, terminalattach.ErrStaleGeneration):
		writeError(w, 409, CodeConflict, err.Error())
	case errors.Is(err, terminalattach.ErrWriterHeld):
		writeError(w, 409, CodeConflict, err.Error())
	case errors.Is(err, terminalattach.ErrNotWriter):
		writeError(w, 403, CodeForbidden, err.Error())
	case errors.Is(err, terminalattach.ErrInvalidRole):
		writeError(w, 400, CodeInvalidRequest, err.Error())
	default:
		writeError(w, 500, CodeInternalError, "terminal attachment unavailable")
	}
}
