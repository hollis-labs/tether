package config

import (
	"path/filepath"
	"testing"
)

func TestMCPUpstreamOwnership(t *testing.T) {
	for _, value := range []string{"", "legacy_proxy", "daemon", "invalid"} {
		got, err := (DaemonConfig{MCPUpstreams: value}).MCPUpstreamOwnership()
		if value == "invalid" {
			if err == nil {
				t.Fatal("unknown ownership accepted")
			}
			continue
		}
		want := value
		if want == "" {
			want = MCPUpstreamsLegacy
		}
		if err != nil || got != want {
			t.Fatalf("%q = %q, %v", value, got, err)
		}
	}
}

func TestOwnershipChangesOnlyAtNewLaunchBoundary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	write(t, filepath.Join(root, "global.yaml"), "daemon:\n  mcp_upstream_ownership: daemon\n")
	got, err := ReadMCPUpstreamOwnership(root, DaemonConfig{})
	if err != nil || got != MCPUpstreamsDaemon {
		t.Fatal(got, err)
	}
	write(t, filepath.Join(root, "global.yaml"), "daemon:\n  mcp_upstream_ownership: unknown\n")
	if _, err := ReadMCPUpstreamOwnership(root, DaemonConfig{}); err == nil {
		t.Fatal("unknown ownership silently fell back")
	}
	// Shared loading remains available to stop/status and other clients.
	if _, err := Load(root); err != nil {
		t.Fatal("shared loader rejected launch-only ownership", err)
	}
}
