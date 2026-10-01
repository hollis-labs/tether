package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestConfineMCPPlant_UnchangedOtherLaunches(t *testing.T) {
	for _, brand := range []string{"claude", "opencode", "codex"} {
		plan := &launch.Plan{ProviderBrand: brand}
		var protected []string
		if brand != "codex" {
			protected = []string{t.TempDir()}
		}
		command, args, err := confineMCPPlant(plan, "mux", []string{"mcp"}, protected)
		if err != nil || command != "mux" || len(args) != 1 || args[0] != "mcp" {
			t.Fatalf("%s: command=%q args=%v err=%v", brand, command, args, err)
		}
		if confinedMCPEnv(plan, protected)[config.MCPConfineRemoteEnv] != "" {
			t.Fatal("remote policy leaked into unchanged launch")
		}
	}
	if confinedMCPEnv(&launch.Plan{ProviderBrand: "codex"}, []string{t.TempDir()})[config.MCPConfineRemoteEnv] != "1" {
		t.Fatal("Codex proxy lacks remote policy")
	}
}

func TestConfineMCPPlant_LocalUpstreamDescendantWrites(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("control-plane protection currently ships on Linux only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "runtime"))
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	root := t.TempDir()
	var protected []string
	for _, name := range []string{"catalog", "run", "state"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		protected = append(protected, dir)
		if err := os.WriteFile(filepath.Join(dir, "existing"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan := &launch.Plan{ProviderBrand: "codex", RepoRoot: root}
	probeCommand, probeArgs, err := confineMCPPlant(plan, "/bin/true", nil, protected)
	if err != nil {
		t.Skipf("sandbox backend unavailable: %v", err)
	}
	if output, err := exec.Command(probeCommand, probeArgs...).CombinedOutput(); err != nil {
		t.Skipf("sandbox namespace unavailable: %v: %s", err, output)
	}
	// The proxy's stdio child in turn spawns a writer, just as an upstream
	// host-shell tool or a session launcher does. It must inherit the mounts.
	script := `exec /bin/sh -c 'for dir do
 test "$(cat "$dir/existing")" = original || exit 1
 if echo changed > "$dir/existing"; then exit 2; fi
 if touch "$dir/new"; then exit 3; fi
 done' upstream "$@"`
	command, args, err := confineMCPPlant(plan, "/bin/sh", append([]string{"-c", script, "proxy"}, protected...), protected)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(command, args...).CombinedOutput(); err != nil {
		t.Fatalf("descendant writes: %v: %s", err, output)
	}
	// Ordinary upstream output outside the protected trees remains writable.
	command, args, err = confineMCPPlant(plan, "/bin/sh", []string{"-c", `echo allowed > "$1"`, "upstream", filepath.Join(root, "output")}, protected)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(command, args...).CombinedOutput(); err != nil {
		t.Fatalf("allowed write: %v: %s", err, output)
	}
	for _, dir := range protected {
		data, err := os.ReadFile(filepath.Join(dir, "existing"))
		if err != nil || string(data) != "original" {
			t.Fatalf("protected file changed: %q, %v", data, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
			t.Fatalf("protected create: %v", err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "output"))
	if err != nil || strings.TrimSpace(string(data)) != "allowed" {
		t.Fatalf("allowed output: %q %v", data, err)
	}
}

func TestConfineMCPPlant_FailsClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux backend")
	}
	t.Setenv("PATH", t.TempDir())
	command, args, err := confineMCPPlant(&launch.Plan{ProviderBrand: "codex", RepoRoot: t.TempDir()}, "/bin/true", nil, []string{t.TempDir()})
	if err == nil || command != "" || args != nil {
		t.Fatalf("unconfined fallback: %q %v %v", command, args, err)
	}
}
