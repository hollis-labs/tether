package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helperInitedStateDir runs mux init --yes in a fresh temp dir and returns
// the state dir path. Used by doctor tests that need a valid install.
func helperInitedStateDir(t *testing.T) string {
	t.Helper()
	stateDir := t.TempDir()
	var dummy bytes.Buffer
	if err := runInit(strings.NewReader(""), &dummy, initOpts{StateDir: stateDir, Yes: true}); err != nil {
		t.Fatalf("helperInitedStateDir: %v", err)
	}
	return stateDir
}

// TestDetectTable verifies mux detect produces the expected columns.
func TestDetectTable(t *testing.T) {
	var out bytes.Buffer
	if err := runDetect(&out, false); err != nil {
		t.Fatalf("runDetect: %v", err)
	}
	for _, hdr := range []string{"BRAND", "FOUND", "SOURCE", "PATH"} {
		if !strings.Contains(out.String(), hdr) {
			t.Errorf("missing column header %q in output:\n%s", hdr, out.String())
		}
	}
	for _, brand := range []string{"claude", "codex", "opencode"} {
		if !strings.Contains(out.String(), brand) {
			t.Errorf("missing brand %q in output:\n%s", brand, out.String())
		}
	}
}

// TestDetectJSON verifies mux detect --json produces valid, correct JSON.
func TestDetectJSON(t *testing.T) {
	var out bytes.Buffer
	if err := runDetect(&out, true); err != nil {
		t.Fatalf("runDetect --json: %v", err)
	}

	var rows []detectRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("JSON parse: %v\noutput:\n%s", err, out.String())
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one row in JSON output")
	}
	brands := make(map[string]bool)
	for _, row := range rows {
		if row.Brand == "" {
			t.Errorf("row missing brand field: %+v", row)
		}
		if row.Source == "" {
			t.Errorf("row missing source field: %+v", row)
		}
		brands[row.Brand] = true
	}
	for _, b := range []string{"claude", "codex", "opencode"} {
		if !brands[b] {
			t.Errorf("brand %q missing from JSON output", b)
		}
	}
}

// TestDetectStubOnPath verifies that a stub binary planted on PATH is reported as found.
func TestDetectStubOnPath(t *testing.T) {
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_CLI_PATH", "")

	var out bytes.Buffer
	if err := runDetect(&out, true); err != nil {
		t.Fatalf("runDetect: %v", err)
	}

	var rows []detectRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("JSON parse: %v", err)
	}
	for _, row := range rows {
		if row.Brand == "claude" {
			if !row.Found {
				t.Errorf("claude should be found with stub on PATH; got %+v", row)
			}
			return
		}
	}
	t.Error("claude row not found in JSON output")
}

// TestDoctorFreshInstall verifies that doctor passes on a freshly mux-init'd install.
func TestDoctorFreshInstall(t *testing.T) {
	stateDir := helperInitedStateDir(t)
	catalogRoot := filepath.Join(stateDir, "catalog")

	var out bytes.Buffer
	doctorErr := runDoctor(&out, stateDir, catalogRoot, false)

	// Doctor is allowed to warn about daemon not running (we're in a test env).
	// Must not fail on catalog / migrations / state-dir checks.
	if doctorErr != nil {
		o := out.String()
		if strings.Contains(o, "✗  catalog") || strings.Contains(o, "✗  migrations") || strings.Contains(o, "✗  state-dir") {
			t.Fatalf("doctor failed on non-daemon check:\n%s\nerr: %v", o, doctorErr)
		}
	}

	o := out.String()
	if !strings.Contains(o, "catalog-present") {
		t.Errorf("expected catalog-present check in output:\n%s", o)
	}
	if !strings.Contains(o, "migrations-current") {
		t.Errorf("expected migrations-current check in output:\n%s", o)
	}
}

// TestDoctorMissingCatalog verifies that doctor fails when catalog is absent.
func TestDoctorMissingCatalog(t *testing.T) {
	stateDir := t.TempDir()
	catalogRoot := filepath.Join(stateDir, "catalog") // doesn't exist

	var out bytes.Buffer
	doctorErr := runDoctor(&out, stateDir, catalogRoot, false)

	if doctorErr == nil {
		t.Error("expected non-zero exit from doctor with missing catalog, got nil")
	}
	if !strings.Contains(out.String(), "✗") {
		t.Errorf("expected at least one fail line in output:\n%s", out.String())
	}
}

// TestDoctorJSON verifies that doctor --json produces valid JSON with expected fields.
func TestDoctorJSON(t *testing.T) {
	stateDir := helperInitedStateDir(t)
	catalogRoot := filepath.Join(stateDir, "catalog")

	var out bytes.Buffer
	_ = runDoctor(&out, stateDir, catalogRoot, true) // ignore exit code for JSON shape test

	var checks []checkResult
	if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
		t.Fatalf("JSON parse: %v\noutput:\n%s", err, out.String())
	}
	if len(checks) == 0 {
		t.Fatal("expected check results in JSON output")
	}
	for _, c := range checks {
		if c.Name == "" {
			t.Errorf("check missing name: %+v", c)
		}
		switch c.Status {
		case statusOK, statusWarn, statusFail:
		default:
			t.Errorf("invalid status %q in check %q", c.Status, c.Name)
		}
	}
}

// TestDoctorStateDirNotWritable verifies that an unwritable state dir is flagged.
func TestDoctorStateDirNotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	result := checkStateDir(stateDir)
	if result.Status == statusOK {
		t.Errorf("expected warn or fail for unwritable state dir, got ok")
	}
}

// TestDoctorLogsDir verifies that a missing logs dir is flagged as warn.
func TestDoctorLogsDir(t *testing.T) {
	stateDir := t.TempDir()

	result := checkLogsDir(stateDir)
	if result.Status == statusOK {
		t.Errorf("expected warn for missing logs dir, got ok; message: %s", result.Message)
	}

	// Now create the logs dir and check again.
	logsDir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	result = checkLogsDir(stateDir)
	if result.Status != statusOK {
		t.Errorf("expected ok for present logs dir, got %s: %s", result.Status, result.Message)
	}
}
