package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newLogsServer(t *testing.T, logsDir string) *httptest.Server {
	t.Helper()
	h := NewHandler(Deps{LogsDir: logsDir})
	return httptest.NewServer(h)
}

// TestLogsEndpointMissingFile verifies that a missing muxd.log returns empty lines, not an error.
func TestLogsEndpointMissingFile(t *testing.T) {
	logsDir := t.TempDir()
	srv := newLogsServer(t, logsDir)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/logs/daemon")
	if err != nil {
		t.Fatalf("GET /logs/daemon: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result DaemonLogsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Lines) != 0 {
		t.Errorf("expected empty lines for missing log file, got %d", len(result.Lines))
	}
}

// TestLogsEndpointDefaultTail verifies that the default tail is 100 lines.
func TestLogsEndpointDefaultTail(t *testing.T) {
	logsDir := t.TempDir()
	var sb strings.Builder
	for i := range 200 {
		sb.WriteString("line ")
		sb.WriteString(strings.Repeat("x", 50))
		_ = i
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(logsDir, "muxd.log"), []byte(sb.String()), 0o640); err != nil {
		t.Fatal(err)
	}

	srv := newLogsServer(t, logsDir)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/logs/daemon")
	if err != nil {
		t.Fatalf("GET /logs/daemon: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var result DaemonLogsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Total != defaultTailLines {
		t.Errorf("expected %d lines (default tail), got %d", defaultTailLines, result.Total)
	}
}

// TestLogsEndpointTailParam verifies that ?tail=N is honored and clamped.
func TestLogsEndpointTailParam(t *testing.T) {
	logsDir := t.TempDir()
	var sb strings.Builder
	for range 50 {
		sb.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(logsDir, "muxd.log"), []byte(sb.String()), 0o640); err != nil {
		t.Fatal(err)
	}

	srv := newLogsServer(t, logsDir)
	defer srv.Close()

	// tail=10 — within bounds.
	resp, err := http.Get(srv.URL + "/logs/daemon?tail=10")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var result DaemonLogsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Total != 10 {
		t.Errorf("expected 10 lines, got %d", result.Total)
	}
	if result.Clamped {
		t.Error("unexpected clamped=true for tail=10")
	}
}

// TestLogsEndpointClampedOverMax verifies that tail > max is clamped.
func TestLogsEndpointClampedOverMax(t *testing.T) {
	logsDir := t.TempDir()
	var sb strings.Builder
	for range maxTailLines + 100 {
		sb.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(logsDir, "muxd.log"), []byte(sb.String()), 0o640); err != nil {
		t.Fatal(err)
	}

	srv := newLogsServer(t, logsDir)
	defer srv.Close()

	resp, err := http.Get(srv.URL + fmt.Sprintf("/logs/daemon?tail=%d", maxTailLines+200))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var result DaemonLogsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Total != maxTailLines {
		t.Errorf("expected %d lines after clamping, got %d", maxTailLines, result.Total)
	}
	if !result.Clamped {
		t.Error("expected clamped=true for tail > max")
	}
}

// TestLogsEndpointNoLogsDir verifies that a missing LogsDir returns 404.
func TestLogsEndpointNoLogsDir(t *testing.T) {
	h := NewHandler(Deps{}) // no LogsDir
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/logs/daemon")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for empty LogsDir, got %d", resp.StatusCode)
	}
}
