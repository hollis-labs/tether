package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/agentops"
	"github.com/hollis-labs/tether/internal/config"
)

func TestResolveScope_KnownValues(t *testing.T) {
	cases := []struct {
		in   string
		want config.Layer
	}{
		{"", config.LayerProject}, // empty defaults to the project layer
		{"project", config.LayerProject},
		{"PROJECT", config.LayerProject},
		{"user", config.LayerUser},
		{"system", config.LayerSystem},
	}
	for _, tc := range cases {
		got, err := agentops.ParseScope(tc.in)
		if err != nil {
			t.Errorf("ParseScope(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseScope(%q) = %s; want %s", tc.in, got, tc.want)
		}
	}
}

func TestResolveScope_UnknownErrors(t *testing.T) {
	if _, err := agentops.ParseScope("garbage"); err == nil {
		t.Fatal("expected error for unknown scope, got nil")
	}
}

// TestAgentsCLI_CreateEditShow exercises create + list + show end-to-end
// against a temp catalog. Drives the cobra commands directly so the wiring
// (flag parsing, output formatting, error paths) gets coverage too.
func TestAgentsCLI_CreateEditShow(t *testing.T) {
	tmp := t.TempDir()
	prevCatalog := catalogPath
	catalogPath = tmp
	t.Cleanup(func() { catalogPath = prevCatalog })

	// create
	resetCreateFlags := func() {
		agentsCreateScope = "system"
		agentsCreateName = ""
		agentsCreateSystem = ""
		agentsCreateAgentPrompt = ""
	}
	resetCreateFlags()
	agentsCreateName = "Test Agent"
	agentsCreateSystem = "you are testy"

	if err := agentsCreateCmd.RunE(agentsCreateCmd, []string{"smoke"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	wantPath := filepath.Join(tmp, "agents", "smoke.yaml")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("create did not produce %s: %v", wantPath, err)
	}

	// duplicate create rejected
	if err := agentsCreateCmd.RunE(agentsCreateCmd, []string{"smoke"}); err == nil {
		t.Fatal("expected duplicate-create error, got nil")
	}

	// list — capture stdout
	out := captureStdout(t, func() {
		if err := agentsListCmd.RunE(agentsListCmd, nil); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	if !strings.Contains(out, "smoke") {
		t.Errorf("list output missing 'smoke': %q", out)
	}
	if !strings.Contains(out, "system") {
		t.Errorf("list output missing 'system' layer column: %q", out)
	}
	if !strings.Contains(out, "Test Agent") {
		t.Errorf("list output missing name: %q", out)
	}

	// show
	out = captureStdout(t, func() {
		if err := agentsShowCmd.RunE(agentsShowCmd, []string{"smoke"}); err != nil {
			t.Fatalf("show: %v", err)
		}
	})
	for _, want := range []string{"id:    smoke", "layer: system", "you are testy"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q: %q", want, out)
		}
	}

	// show missing
	if err := agentsShowCmd.RunE(agentsShowCmd, []string{"no-such-agent"}); err == nil {
		t.Fatal("expected error for missing agent, got nil")
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns the
// captured text. Used for cobra command tests because the commands write
// straight to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()
	fn()
	_ = w.Close()
	<-done
	os.Stdout = old
	return buf.String()
}
