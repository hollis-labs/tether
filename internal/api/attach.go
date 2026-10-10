package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
)

// handleAttach streams the named session's live PTY output as an unframed
// byte stream. The response uses application/octet-stream and flushes after
// each chunk so curl-style consumers see data immediately. The response
// ends when the session exits (broker closes) or the client disconnects
// (ctx cancels).
//
// Optional ?since_seq=N param lets callers resume where they left off;
// the server replays only bytes with offset > N still available in the
// attach ring. X-Tether-Attach-Oldest-Offset and -Next-Offset describe the
// retained admission window; they are byte boundaries, not event sequences.
// X-Tether-Attach-Evicted is true when the request precedes that window.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request, id string) {
	var sinceSeq int64
	if raw := r.URL.Query().Get("since_seq"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "since_seq must be a non-negative integer")
			return
		}
		sinceSeq = n
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	fw := &flushWriter{w: w, f: flusher}
	if service, ok := s.Service.(attachSnapshotService); ok {
		started := false
		err := service.AttachSessionWithSnapshot(r.Context(), id, fw, sinceSeq, func(snapshot agentsessions.AttachSnapshot) error {
			if err := r.Context().Err(); err != nil {
				return err
			}
			w.Header().Set("X-Tether-Attach-Oldest-Offset", strconv.FormatInt(snapshot.OldestOffset, 10))
			w.Header().Set("X-Tether-Attach-Next-Offset", strconv.FormatInt(snapshot.NextOffset, 10))
			w.Header().Set("X-Tether-Attach-Evicted", strconv.FormatBool(sinceSeq < snapshot.OldestOffset))
			w.Header().Set("X-Tether-Attach-Cursor", "byte-offset")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			started = true
			return nil
		})
		if err != nil && !started {
			if errors.Is(err, agentsessions.ErrSessionNotRunning) {
				writeError(w, http.StatusConflict, CodeConflict, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, CodeInternalError, "attach unavailable")
			}
		}
		return
	}
	// Compatibility for older injected consumers. This path supplies no
	// invented window; production's adapter implements the snapshot seam.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	if err := s.Service.AttachSession(r.Context(), id, fw, sinceSeq); err != nil {
		if errors.Is(err, agentsessions.ErrSessionNotRunning) {
			// Headers already sent — best we can do is close the stream.
			return
		}
		// Other errors also silently close; the client sees EOF.
	}
}

type attachSnapshotService interface {
	AttachSessionWithSnapshot(context.Context, string, io.Writer, int64, func(agentsessions.AttachSnapshot) error) error
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
