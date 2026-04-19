package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/chrispian/agent-mux/internal/runtime"
)

// handleAttach streams the named session's live PTY output as an unframed
// byte stream. The response uses application/octet-stream and flushes after
// each chunk so curl-style consumers see data immediately. The response
// ends when the session exits (broker closes) or the client disconnects
// (ctx cancels).
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request, id string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	fw := &flushWriter{w: w, f: flusher}
	if err := s.Service.AttachSession(r.Context(), id, fw); err != nil {
		if errors.Is(err, runtime.ErrSessionNotRunning) {
			// Headers already sent — best we can do is close the stream.
			return
		}
		// Other errors also silently close; the client sees EOF.
	}
}

// flushWriter is an io.Writer that flushes after each Write so the attach
// stream reaches the client promptly. Not concurrency-safe; only the attach
// goroutine writes to it.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}
