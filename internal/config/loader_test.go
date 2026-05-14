package config

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hollis-labs/go-sandbox/sandbox"
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
	if got := cat.Providers["claude-code"].EffectiveRuntimeKind(); got != RuntimeKindStreamingStdio {
		t.Fatalf("claude-code runtime_kind = %q, want %q", got, RuntimeKindStreamingStdio)
	}
	if got := cat.Providers["claude-pty"].EffectiveRuntimeKind(); got != RuntimeKindPTY {
		t.Fatalf("claude-pty runtime_kind = %q, want %q", got, RuntimeKindPTY)
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

func TestProviderRuntimeDefaults_BackCompat(t *testing.T) {
	tests := []struct {
		name string
		in   Provider
		want string
	}{
		{name: "streaming bootstrap", in: Provider{Bootstrap: BootstrapSpec{Mode: RuntimeKindStreamingStdio}}, want: RuntimeKindStreamingStdio},
		{name: "jsonrpc bootstrap", in: Provider{Bootstrap: BootstrapSpec{Mode: RuntimeKindJSONRPCStdio}}, want: RuntimeKindJSONRPCStdio},
		{name: "api type", in: Provider{Type: "api"}, want: RuntimeKindAPI},
		{name: "default subprocess", in: Provider{Type: "cli"}, want: RuntimeKindSubprocess},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.EffectiveRuntimeKind(); got != tt.want {
				t.Fatalf("EffectiveRuntimeKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidate_UnsupportedRuntimeKind(t *testing.T) {
	cat := &Catalog{
		Projects:  map[string]Project{},
		Agents:    map[string]Agent{},
		Providers: map[string]Provider{"p": {ID: "p", Type: "cli", Command: "echo", RuntimeKind: "websocket"}},
		Launches:  map[string]Launch{},
	}
	if err := cat.Validate(); err == nil {
		t.Fatal("expected unsupported runtime_kind error, got nil")
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
