package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newFSServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(NewHandler(Deps{}))
}

// TestFSValidateExistingFile verifies that an existing executable file is detected.
func TestFSValidateExistingFile(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "mybinary")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := newFSServer(t)
	defer srv.Close()

	body, _ := json.Marshal(FSValidateRequest{Path: bin, Kind: "executable"})
	resp, err := http.Post(srv.URL+"/fs/validate", "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /fs/validate: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var result FSValidateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Exists {
		t.Error("expected exists=true for existing file")
	}
	if result.Executable == nil || !*result.Executable {
		t.Error("expected executable=true for +x file")
	}
	if result.Resolved == "" {
		t.Error("expected non-empty resolved path")
	}
}

// TestFSValidateMissingPath verifies that a missing path returns exists=false.
func TestFSValidateMissingPath(t *testing.T) {
	srv := newFSServer(t)
	defer srv.Close()

	body, _ := json.Marshal(FSValidateRequest{Path: "/nonexistent/path/binary", Kind: "executable"})
	resp, err := http.Post(srv.URL+"/fs/validate", "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /fs/validate: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var result FSValidateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Exists {
		t.Error("expected exists=false for missing path")
	}
}

// TestFSValidateEmptyPath verifies that an empty path returns exists=false with a note.
func TestFSValidateEmptyPath(t *testing.T) {
	srv := newFSServer(t)
	defer srv.Close()

	body, _ := json.Marshal(FSValidateRequest{Path: "", Kind: "executable"})
	resp, err := http.Post(srv.URL+"/fs/validate", "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /fs/validate: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result FSValidateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Exists {
		t.Error("expected exists=false for empty path")
	}
}

// TestFSValidateDir verifies dir kind detection.
func TestFSValidateDir(t *testing.T) {
	dir := t.TempDir()
	srv := newFSServer(t)
	defer srv.Close()

	body, _ := json.Marshal(FSValidateRequest{Path: dir, Kind: "dir"})
	resp, err := http.Post(srv.URL+"/fs/validate", "application/json", bytes.NewReader(body)) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /fs/validate: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var result FSValidateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Exists {
		t.Error("expected exists=true for existing dir")
	}
}

// TestFSDetectUnknownBrand verifies detect returns found=false for unknown brand.
func TestFSDetectUnknownBrand(t *testing.T) {
	srv := newFSServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/fs/detect?brand=unknownxyz")
	if err != nil {
		t.Fatalf("GET /fs/detect: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var result FSDetectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Found {
		t.Error("expected found=false for unknown brand")
	}
	if result.Brand != "unknownxyz" {
		t.Errorf("expected brand=unknownxyz, got %q", result.Brand)
	}
}

// TestFSDetectKnownBrand verifies detect returns a result for a known brand.
func TestFSDetectKnownBrand(t *testing.T) {
	srv := newFSServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/fs/detect?brand=claude")
	if err != nil {
		t.Fatalf("GET /fs/detect: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result FSDetectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Brand != "claude" {
		t.Errorf("expected brand=claude, got %q", result.Brand)
	}
	// found may be true or false depending on the test machine — just ensure Source is set.
	if result.Source == "" {
		t.Error("expected non-empty source field")
	}
}
