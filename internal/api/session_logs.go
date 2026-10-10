package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"syscall"

	"github.com/hollis-labs/tether/internal/store"
)

// MaxSessionLogBytes bounds each request, including the default tail snapshot.
const MaxSessionLogBytes = 64 << 10

// SessionLogResponse uses base64 JSON byte data so invalid UTF-8 is preserved.
// Offsets belong only to this log generation, never to an event/PTY sequence.
type SessionLogResponse struct {
	Data       []byte `json:"data"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	Size       int64  `json:"size"`
	Generation string `json:"generation"`
}

type sessionLogVersion struct {
	info       os.FileInfo
	prefix     []byte
	generation string
	used       uint64
}

// A bounded cache permits portable os.SameFile replacement detection without
// exposing host inode metadata or persisting cursor state. Eviction/restart
// invalidates continuation safely. Same-uid unseen truncate-and-regrow with
// identical prefix cannot be distinguished from append (see remote docs).
var sessionLogVersions = struct {
	sync.Mutex
	clock uint64
	items map[string]sessionLogVersion
}{items: make(map[string]sessionLogVersion)}

func sessionLogGeneration(key string, f *os.File, info os.FileInfo) (string, error) {
	prefix := make([]byte, min(info.Size(), 4096))
	if _, err := f.ReadAt(prefix, 0); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	sessionLogVersions.Lock()
	defer sessionLogVersions.Unlock()
	old, ok := sessionLogVersions.items[key]
	if !ok || !os.SameFile(old.info, info) || info.Size() < old.info.Size() || !bytes.HasPrefix(prefix, old.prefix) {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", err
		}
		old.generation = hex.EncodeToString(nonce[:])
	}
	if !ok && len(sessionLogVersions.items) >= 512 {
		var oldestKey string
		oldest := ^uint64(0)
		for candidate, version := range sessionLogVersions.items {
			if version.used < oldest {
				oldestKey, oldest = candidate, version.used
			}
		}
		delete(sessionLogVersions.items, oldestKey)
	}
	sessionLogVersions.clock++
	old.info, old.prefix, old.used = info, prefix, sessionLogVersions.clock
	sessionLogVersions.items[key] = old
	return old.generation, nil
}

func (s *Server) handleSessionLog(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	limit := MaxSessionLogBytes
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > MaxSessionLogBytes {
			writeError(w, 400, CodeInvalidRequest, "limit must be between 1 and 65536 bytes")
			return
		}
		limit = n
	}
	var offset int64
	if q.Has("offset") {
		n, err := strconv.ParseInt(q.Get("offset"), 10, 64)
		if err != nil || n < 0 {
			writeError(w, 400, CodeInvalidRequest, "offset must be a non-negative byte offset")
			return
		}
		offset = n
	}
	if q.Has("generation") && (!q.Has("offset") || q.Get("generation") == "") {
		writeError(w, 400, CodeInvalidRequest, "generation requires an offset and a non-empty value")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	row, err := s.Service.GetSession(id)
	if errors.Is(err, store.ErrSessionNotFound) || err == nil && (row == nil || row.Workspace == "") {
		writeError(w, 404, CodeNotFound, "session log unavailable")
		return
	}
	if err != nil {
		writeError(w, 500, CodeInternalError, "session lookup failed")
		return
	}
	root, err := os.OpenRoot(row.Workspace)
	if err != nil {
		writeSessionLogError(w, err)
		return
	}
	defer func() { _ = root.Close() }()
	// Stat first refuses special files; Root.Open confines symlink traversal
	// even if a workspace component changes between stat and open.
	info, err := root.Stat("logs/session.log")
	if err != nil {
		writeSessionLogError(w, err)
		return
	}
	if !info.Mode().IsRegular() {
		writeError(w, 409, CodeConflict, "session log is not a regular file")
		return
	}
	f, err := root.OpenFile("logs/session.log", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		writeSessionLogError(w, err)
		return
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, 409, CodeConflict, "session log unavailable")
		return
	}
	generation, err := sessionLogGeneration(row.Workspace+"\x00"+id, f, info)
	if err != nil {
		writeError(w, 500, CodeInternalError, "session log read failed")
		return
	}
	if q.Has("generation") && q.Get("generation") != generation || q.Has("offset") && offset > info.Size() {
		writeError(w, 409, "log_changed", "log changed; request a new snapshot")
		return
	}
	if !q.Has("offset") {
		offset = max(0, info.Size()-int64(limit))
	}
	data := make([]byte, min(int64(limit), info.Size()-offset))
	n, err := f.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(w, 500, CodeInternalError, "session log read failed")
		return
	}
	after, err := f.Stat()
	if err != nil || after.Size() < info.Size() || n != len(data) {
		writeError(w, 409, "log_changed", "log changed; request a new snapshot")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	writeJSON(w, http.StatusOK, SessionLogResponse{Data: data, Offset: offset, NextOffset: offset + int64(n), Size: info.Size(), Generation: generation})
}

func writeSessionLogError(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, 404, CodeNotFound, "session log unavailable")
	} else {
		writeError(w, 409, CodeConflict, "session log inaccessible")
	}
}
