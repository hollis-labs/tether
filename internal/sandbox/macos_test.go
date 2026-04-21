//go:build darwin

package sandbox_test

import (
	"strings"
	"testing"

	"github.com/chrispian/agent-mux/internal/sandbox"
)

func TestBuildSBPL_DenyNetwork(t *testing.T) {
	p := sandbox.Profile{
		ID: "workspace-only",
		FS: sandbox.FSSpec{
			Write: []string{"workspace"},
			Read:  []string{"workspace"},
		},
		Net:        false,
		Subprocess: true,
	}
	sbpl, err := sandbox.BuildSBPL(p, "/tmp/ws/abc123")
	if err != nil {
		t.Fatalf("BuildSBPL: %v", err)
	}
	// Must be a valid SBPL start
	if !strings.HasPrefix(strings.TrimSpace(sbpl), "(version 1)") {
		t.Errorf("SBPL missing version header; got: %q", truncate(sbpl, 80))
	}
	// Network deny must appear
	if !strings.Contains(sbpl, "(deny network*") {
		t.Errorf("expected (deny network*) in SBPL:\n%s", sbpl)
	}
	// Workspace write-allow must appear
	if !strings.Contains(sbpl, "/tmp/ws/abc123") {
		t.Errorf("expected workspace path in SBPL:\n%s", sbpl)
	}
}

func TestBuildSBPL_AllowNetwork(t *testing.T) {
	p := sandbox.Profile{
		ID:         "workspace-plus-net",
		Net:        true,
		Subprocess: true,
	}
	sbpl, err := sandbox.BuildSBPL(p, "/tmp/ws/xyz")
	if err != nil {
		t.Fatalf("BuildSBPL: %v", err)
	}
	if strings.Contains(sbpl, "(deny network*") {
		t.Errorf("expected no network deny for net=true; got:\n%s", sbpl)
	}
}

func TestBuildSBPL_DenySubprocess(t *testing.T) {
	p := sandbox.Profile{
		ID:         "no-subprocess",
		Net:        true,
		Subprocess: false,
	}
	sbpl, err := sandbox.BuildSBPL(p, "/tmp/ws/xyz")
	if err != nil {
		t.Fatalf("BuildSBPL: %v", err)
	}
	if !strings.Contains(sbpl, "(deny process*") {
		t.Errorf("expected (deny process*) for subprocess=false; got:\n%s", sbpl)
	}
}

func TestBuildSBPL_DenyPaths(t *testing.T) {
	p := sandbox.Profile{
		ID: "locked",
		FS: sandbox.FSSpec{
			Deny: []string{"${HOME}/.ssh"},
		},
		Net:        false,
		Subprocess: true,
	}
	sbpl, err := sandbox.BuildSBPL(p, "/tmp/ws/abc")
	if err != nil {
		t.Fatalf("BuildSBPL: %v", err)
	}
	// .ssh deny must be present (path expanded)
	if !strings.Contains(sbpl, ".ssh") {
		t.Errorf("expected .ssh deny in SBPL:\n%s", sbpl)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
