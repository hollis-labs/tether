package api

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

const (
	defaultTailLines = 100
	maxTailLines     = 500
	minTailLines     = 1
)

// DaemonLogsResponse is the JSON shape for GET /logs/daemon.
type DaemonLogsResponse struct {
	Lines   []string `json:"lines"`
	Total   int      `json:"total"`
	Clamped bool     `json:"clamped,omitempty"` // true when the requested tail was capped
}

func (s *Server) registerLogsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/logs/daemon", s.handleDaemonLogs)
}

func (s *Server) handleDaemonLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.LogsDir == "" {
		http.NotFound(w, r)
		return
	}

	tail := defaultTailLines
	clamped := false
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < minTailLines {
			n = minTailLines
		}
		if n > maxTailLines {
			n = maxTailLines
			clamped = true
		}
		tail = n
	}

	logPath := filepath.Join(s.LogsDir, "muxd.log") //nolint:gosec // path from trusted config
	lines, err := tailFile(logPath, tail)
	if err != nil {
		if os.IsNotExist(err) {
			// Log file doesn't exist yet — return empty response.
			writeJSON(w, http.StatusOK, DaemonLogsResponse{Lines: []string{}, Total: 0})
			return
		}
		http.Error(w, fmt.Sprintf("read log: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, DaemonLogsResponse{
		Lines:   lines,
		Total:   len(lines),
		Clamped: clamped,
	})
}

// tailFile returns the last n lines of path. Lines are returned in
// chronological order (oldest first). It reads the whole file, which is safe
// because muxd.log is rotation-bounded at 10 MiB.
func tailFile(path string, n int) ([]string, error) {
	//nolint:gosec // G304: path from trusted config (LogsDir), not user input.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	var all []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	if len(all) <= n {
		return all, nil
	}
	return all[len(all)-n:], nil
}
