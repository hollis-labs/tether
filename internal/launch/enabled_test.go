package launch

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

// copyExample copies examples/catalog so a test can edit the YAML on disk and
// load it the way the daemon does.
func copyExample(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "catalog")
	src, _ := filepath.Abs(filepath.Join("..", "..", "examples", "catalog"))
	if out, err := exec.Command("cp", "-r", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return dst
}

func setYAMLLine(t *testing.T, path, line string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, []byte("\n"+line+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// enabled: false in a launch or agent file blocks a new launch, names the
// disabled entity, and flipping it back re-enables it.
func TestResolveRefusesDisabledLaunchAndAgent(t *testing.T) {
	root := copyExample(t)
	load := func() *config.Catalog {
		cat, err := config.Load(root)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return cat
	}
	if _, err := Resolve(load(), Input{LaunchID: "demo-launch", CatalogRoot: root}); err != nil {
		t.Fatalf("enabled by default: %v", err)
	}

	launchFile := filepath.Join(root, "launches", "demo-launch.yaml")
	setYAMLLine(t, launchFile, "enabled: false")
	_, err := Resolve(load(), Input{LaunchID: "demo-launch", CatalogRoot: root})
	if !errors.Is(err, ErrDisabled) || !strings.Contains(err.Error(), `launch "demo-launch"`) {
		t.Fatalf("disabled launch: %v", err)
	}
	if errors.Is(err, ErrLaunchNotFound) {
		t.Fatal("a disabled launch is not a missing one")
	}

	b, _ := os.ReadFile(launchFile)
	if err := os.WriteFile(launchFile, []byte(strings.ReplaceAll(string(b), "enabled: false", "enabled: true")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(load(), Input{LaunchID: "demo-launch", CatalogRoot: root}); err != nil {
		t.Fatalf("re-enabled launch: %v", err)
	}

	setYAMLLine(t, filepath.Join(root, "agents", "demo-agent.yaml"), "enabled: false")
	_, err = Resolve(load(), Input{LaunchID: "demo-launch", CatalogRoot: root})
	if !errors.Is(err, ErrDisabled) || !strings.Contains(err.Error(), `agent "demo-agent"`) {
		t.Fatalf("disabled agent: %v", err)
	}
}
