package config

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/chrispian/agent-mux/internal/sandbox"
)

func TestLoadExampleCatalog(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	catalogRoot := filepath.Join(filepath.Dir(file), "..", "..", "examples", "catalog")
	cat, err := Load(catalogRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cat.Projects["demo"]; !ok {
		t.Fatalf("missing demo project")
	}
	if _, ok := cat.Agents["demo-agent"]; !ok {
		t.Fatalf("missing demo-agent")
	}
	if _, ok := cat.Providers["claude-code"]; !ok {
		t.Fatalf("missing claude-code provider")
	}
	if _, ok := cat.Launches["demo-launch"]; !ok {
		t.Fatalf("missing demo-launch")
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Daemon defaults are applied when the catalog omits the block.
	if got, want := cat.Global.Daemon.ListenAddr, "unix:~/.agent-mux/run/muxd.sock"; got != want {
		t.Errorf("daemon.listen_addr = %q, want default %q", got, want)
	}
	if got, want := cat.Global.Daemon.PIDFile, "~/.agent-mux/run/muxd.pid"; got != want {
		t.Errorf("daemon.pid_file = %q, want default %q", got, want)
	}
	if got, want := cat.Global.Daemon.ShutdownTimeout, "10s"; got != want {
		t.Errorf("daemon.shutdown_timeout = %q, want default %q", got, want)
	}
}

func TestLoad_SandboxProfiles(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	catalogRoot := filepath.Join(filepath.Dir(file), "..", "..", "examples", "catalog")
	cat, err := Load(catalogRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Three seed profiles must be present in the examples catalog.
	for _, id := range []string{"workspace-only", "workspace-plus-net", "unrestricted"} {
		if _, ok := cat.SandboxProfiles[id]; !ok {
			t.Errorf("seed profile %q not loaded", id)
		}
	}
}

func TestValidate_UnknownSandboxProfile(t *testing.T) {
	cat := &Catalog{
		Projects:  map[string]Project{},
		Agents:    map[string]Agent{"a": {ID: "a", Permissions: AgentPermissions{DefaultSandbox: "no-such-profile"}}},
		Providers: map[string]Provider{},
		Launches:  map[string]Launch{},
		SandboxProfiles: map[string]sandbox.Profile{
			"workspace-only": {ID: "workspace-only"},
		},
	}
	if err := cat.Validate(); err == nil {
		t.Error("expected error for unknown sandbox profile reference, got nil")
	}
}

func TestValidate_KnownSandboxProfile(t *testing.T) {
	cat := &Catalog{
		Projects:  map[string]Project{},
		Agents:    map[string]Agent{"a": {ID: "a", Permissions: AgentPermissions{DefaultSandbox: "workspace-only"}}},
		Providers: map[string]Provider{},
		Launches:  map[string]Launch{},
		SandboxProfiles: map[string]sandbox.Profile{
			"workspace-only": {ID: "workspace-only"},
		},
	}
	if err := cat.Validate(); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

func TestValidate_EmptySandboxProfile(t *testing.T) {
	cat := &Catalog{
		Projects:        map[string]Project{},
		Agents:          map[string]Agent{"a": {ID: "a", Permissions: AgentPermissions{DefaultSandbox: ""}}},
		Providers:       map[string]Provider{},
		Launches:        map[string]Launch{},
		SandboxProfiles: map[string]sandbox.Profile{},
	}
	if err := cat.Validate(); err != nil {
		t.Errorf("empty sandbox profile should not error: %v", err)
	}
}

func TestApplyDaemonDefaults_OverrideRespected(t *testing.T) {
	d := DaemonConfig{
		ListenAddr:      "tcp:127.0.0.1:9999",
		PIDFile:         "/tmp/muxd.pid",
		ShutdownTimeout: "30s",
	}
	applyDaemonDefaults(&d)
	if d.ListenAddr != "tcp:127.0.0.1:9999" {
		t.Errorf("listen_addr overwritten: %q", d.ListenAddr)
	}
	if d.PIDFile != "/tmp/muxd.pid" {
		t.Errorf("pid_file overwritten: %q", d.PIDFile)
	}
	if d.ShutdownTimeout != "30s" {
		t.Errorf("shutdown_timeout overwritten: %q", d.ShutdownTimeout)
	}
}
