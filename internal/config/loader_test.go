package config

import (
	"os"
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
	if got, want := cat.Global.Daemon.ListenAddr, "unix:~/.tether/run/muxd.sock"; got != want {
		t.Errorf("daemon.listen_addr = %q, want default %q", got, want)
	}
	if got, want := cat.Global.Daemon.PIDFile, "~/.tether/run/muxd.pid"; got != want {
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

func TestValidate_LaunchInjectionRejectsUnsafeRelPaths(t *testing.T) {
	base := func(relPath string, overlay bool) *Catalog {
		injection := LaunchInjection{}
		if overlay {
			injection.BootDirOverlay = []InjectedFile{{RelPath: relPath, Content: "overlay"}}
		} else {
			injection.NativeFiles = []InjectedFile{{Kind: "raw", RelPath: relPath, Content: "native"}}
		}
		return &Catalog{
			Projects:  map[string]Project{"p": {ID: "p"}},
			Agents:    map[string]Agent{"a": {ID: "a"}},
			Providers: map[string]Provider{"provider": {ID: "provider", Type: "cli", Command: "echo"}},
			Launches: map[string]Launch{
				"launch": {ID: "launch", Project: "p", Agent: "a", Provider: "provider", Injection: injection},
			},
		}
	}

	for _, tt := range []struct {
		name    string
		relPath string
		overlay bool
	}{
		{name: "overlay parent traversal", relPath: "../CLAUDE.md", overlay: true},
		{name: "overlay nested traversal", relPath: "safe/../../CLAUDE.md", overlay: true},
		{name: "overlay absolute", relPath: "/tmp/CLAUDE.md", overlay: true},
		{name: "native parent traversal", relPath: "../.mux/context.md", overlay: false},
		{name: "native home expansion", relPath: "~/context.md", overlay: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := base(tt.relPath, tt.overlay).Validate(); err == nil {
				t.Fatalf("expected unsafe rel_path %q to fail validation", tt.relPath)
			}
		})
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

// TestLoadLayered_ResolvesUserAndProjectAgents builds a system catalog whose
// launches reference agents that live only in the user and project discovery
// layers. Plain Load cannot see those agents, so cat.Validate would fail with
// "references unknown agent"; LoadLayered must overlay them so validation
// passes. This is the regression guard for the catalog-profile launch bug.
func TestLoadLayered_ResolvesUserAndProjectAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoRoot := t.TempDir()
	catalogRoot := t.TempDir()

	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// System catalog: a project, a provider, and one launch per layered agent.
	write(filepath.Join(catalogRoot, "global.yaml"), "version: 0.1.0\n")
	write(filepath.Join(catalogRoot, "projects", "p.yaml"),
		"id: p\nname: P\nrepo_root: "+repoRoot+"\n")
	write(filepath.Join(catalogRoot, "providers", "cli.yaml"),
		"id: cli\ntype: cli\ncommand: echo\n")
	write(filepath.Join(catalogRoot, "launches", "user-launch.yaml"),
		"id: user-launch\nproject: p\nagent: user-agent\nprovider: cli\n")
	write(filepath.Join(catalogRoot, "launches", "project-launch.yaml"),
		"id: project-launch\nproject: p\nagent: project-agent\nprovider: cli\n")

	// user-agent lives only in the user layer (~/.tether/agents/).
	write(filepath.Join(home, ".tether", "agents", "user-agent.yaml"),
		"id: user-agent\nname: User Agent\n")
	// project-agent lives only in the project layer (<repo>/.tether/agents/).
	write(filepath.Join(repoRoot, ".tether", "agents", "project-agent.yaml"),
		"id: project-agent\nname: Project Agent\n")

	// Plain Load cannot see the layered agents — validation must fail.
	plain, err := Load(catalogRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := plain.Validate(); err == nil {
		t.Fatal("Load + Validate: expected unknown-agent error, got nil")
	}

	// LoadLayered overlays them — validation must pass.
	cat, err := LoadLayered(catalogRoot)
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if _, ok := cat.Agents["user-agent"]; !ok {
		t.Error("LoadLayered: missing user-layer agent")
	}
	if _, ok := cat.Agents["project-agent"]; !ok {
		t.Error("LoadLayered: missing project-layer agent")
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("LoadLayered + Validate: %v", err)
	}
}
