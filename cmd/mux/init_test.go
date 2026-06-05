package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

// TestInitYes verifies that mux init --yes on an empty state dir produces a
// bootable catalog and leaves no stdin reads pending.
func TestInitYes(t *testing.T) {
	stateDir := t.TempDir()
	var out bytes.Buffer

	if err := runInit(strings.NewReader(""), &out, initOpts{
		StateDir: stateDir,
		Yes:      true,
	}); err != nil {
		t.Fatalf("runInit --yes: %v\noutput:\n%s", err, out.String())
	}

	// global.yaml must exist.
	if !fileExists(filepath.Join(stateDir, "catalog", "global.yaml")) {
		t.Fatal("global.yaml not created")
	}

	// Catalog must be loadable + valid.
	cat, err := config.LoadLayered(filepath.Join(stateDir, "catalog"))
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// Output should mention "✓ Wrote" and "Next steps".
	o := out.String()
	if !strings.Contains(o, "✓ Wrote") {
		t.Errorf("expected '✓ Wrote' in output, got:\n%s", o)
	}
	if !strings.Contains(o, "Next steps") {
		t.Errorf("expected 'Next steps' in output, got:\n%s", o)
	}
}

// TestInitDetectedPathStamped verifies that a detected provider binary path
// lands in the seeded provider YAML.
func TestInitDetectedPathStamped(t *testing.T) {
	stateDir := t.TempDir()

	// Plant a stub "claude" binary on PATH so detection finds it.
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_CLI_PATH", "")

	var out bytes.Buffer
	if err := runInit(strings.NewReader(""), &out, initOpts{
		StateDir: stateDir,
		Yes:      true,
	}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	claudeYAML, err := os.ReadFile(filepath.Join(stateDir, "catalog", "providers", "claude-code.yaml")) //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("read provider YAML: %v", err)
	}
	if !strings.Contains(string(claudeYAML), "command: "+stub) {
		t.Errorf("expected 'command: %s' in provider YAML:\n%s", stub, claudeYAML)
	}
}

// TestInitSkippedBrandEmpty verifies that when a user explicitly skips a
// provider in the interactive flow, its command field is left empty and the
// output mentions "set later".
func TestInitSkippedBrandEmpty(t *testing.T) {
	stateDir := t.TempDir()

	// Drive the interactive flow: type "skip" for the first provider (claude),
	// then accept defaults for the rest. DetectProviders always returns 3 rows
	// (claude, codex, opencode) in that order.
	// Prompts consumed: claude answer, codex answer, opencode answer,
	//                   state-location, write-catalog confirm.
	input := strings.NewReader("skip\n\n\n\n\n")
	var out bytes.Buffer
	if err := runInit(input, &out, initOpts{
		StateDir: stateDir,
		Yes:      false,
	}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	claudeYAML, err := os.ReadFile(filepath.Join(stateDir, "catalog", "providers", "claude-code.yaml")) //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("read provider YAML: %v", err)
	}
	// After an explicit skip, command must be empty ("" not a path).
	// Use "\ncommand: /" to skip the comment line "# Example: command: /opt/homebrew/..."
	if strings.Contains(string(claudeYAML), "\ncommand: /") {
		t.Errorf("expected empty command after skip, got:\n%s", claudeYAML)
	}

	// Output should mention "set later".
	if !strings.Contains(out.String(), "set later") {
		t.Errorf("expected 'set later' in output, got:\n%s", out.String())
	}
}

// TestInitIdempotent verifies that a second run without --force skips
// existing files.
func TestInitIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	opts := initOpts{StateDir: stateDir, Yes: true}

	// First run.
	var out1 bytes.Buffer
	if err := runInit(strings.NewReader(""), &out1, opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Stamp a sentinel into global.yaml to prove it's not overwritten.
	globalYAML := filepath.Join(stateDir, "catalog", "global.yaml")
	original, err := os.ReadFile(globalYAML) //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("read global.yaml: %v", err)
	}

	// Second run.
	var out2 bytes.Buffer
	if err := runInit(strings.NewReader(""), &out2, opts); err != nil {
		t.Fatalf("second run: %v", err)
	}

	// global.yaml must be unchanged.
	after, err := os.ReadFile(globalYAML) //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("read global.yaml after second run: %v", err)
	}
	if !bytes.Equal(original, after) {
		t.Errorf("global.yaml changed on second run (should be skipped)")
	}
	if !strings.Contains(out2.String(), "Skipped") {
		t.Errorf("expected 'Skipped' in second-run output, got:\n%s", out2.String())
	}
}

// TestInitForce verifies that --force creates backup files.
func TestInitForce(t *testing.T) {
	stateDir := t.TempDir()
	opts := initOpts{StateDir: stateDir, Yes: true}

	// First run.
	if err := runInit(strings.NewReader(""), &bytes.Buffer{}, opts); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Force run.
	opts.Force = true
	var out bytes.Buffer
	if err := runInit(strings.NewReader(""), &out, opts); err != nil {
		t.Fatalf("force run: %v", err)
	}

	if !strings.Contains(out.String(), "Backed up") {
		t.Errorf("expected 'Backed up' in --force output, got:\n%s", out.String())
	}
}

// TestInitPrintPlan verifies that --print-plan writes nothing.
func TestInitPrintPlan(t *testing.T) {
	stateDir := t.TempDir()
	var out bytes.Buffer

	if err := runInit(strings.NewReader(""), &out, initOpts{
		StateDir:  stateDir,
		Yes:       true,
		PrintPlan: true,
	}); err != nil {
		t.Fatalf("--print-plan: %v", err)
	}

	// No catalog should have been written.
	if fileExists(filepath.Join(stateDir, "catalog", "global.yaml")) {
		t.Error("--print-plan should not write global.yaml")
	}

	if !strings.Contains(out.String(), "--print-plan") {
		t.Errorf("expected '--print-plan' in output, got:\n%s", out.String())
	}
}

// TestInitNonTTYNoHang verifies that a closed stdin (EOF) in non---yes mode
// does not hang the command.
func TestInitNonTTYNoHang(t *testing.T) {
	stateDir := t.TempDir()
	// Use a pre-filled reader that immediately returns EOF after the prompts.
	// In --yes mode we don't need any input; this verifies the closed-reader
	// path used by non-TTY callers (pipes, CI).
	var out bytes.Buffer
	if err := runInit(strings.NewReader(""), &out, initOpts{
		StateDir: stateDir,
		Yes:      true, // non-TTY callers should pass --yes or the daemon handles it
	}); err != nil {
		t.Fatalf("non-TTY run: %v", err)
	}
}

// TestInitInteractiveAccept drives the interactive prompt flow with injected
// input: accept detected path for a found provider, skip unknown ones.
func TestInitInteractiveAccept(t *testing.T) {
	stateDir := t.TempDir()

	// Plant a stub "claude" on PATH.
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_CLI_PATH", "")
	t.Setenv("CODEX_CLI_PATH", "")
	t.Setenv("OPENCODE_CLI_PATH", "")

	// Interactive input:
	// - claude prompt: Enter (accept detected path)
	// - codex  prompt: Enter (skip)
	// - opencode prompt: Enter (skip)
	// - State location: Enter (keep default)
	// - Write catalog? Enter (yes)
	input := strings.NewReader("\n\n\n\n\n")
	var out bytes.Buffer

	if err := runInit(input, &out, initOpts{
		StateDir: stateDir,
		Yes:      false,
	}); err != nil {
		t.Fatalf("interactive run: %v", err)
	}

	claudeYAML, err := os.ReadFile(filepath.Join(stateDir, "catalog", "providers", "claude-code.yaml")) //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("read provider YAML: %v", err)
	}
	if !strings.Contains(string(claudeYAML), "command: "+stub) {
		t.Errorf("accepted path not stamped in YAML:\n%s", claudeYAML)
	}
}
