package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func reloadService(t *testing.T) (*Service, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "catalog")
	src, _ := filepath.Abs(filepath.Join("..", "..", "examples", "catalog"))
	if out, err := exec.Command("cp", "-r", src, root).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cat, err := config.LoadLayered(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Catalog: cat, CatalogRoot: root}, root
}

func writeCat(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A new agent and a launch for it, written to disk after the daemon started,
// cannot launch until the catalog is reloaded; after the reload they can, with
// no restart.
func TestReloadCatalogPicksUpNewAgentWithoutRestart(t *testing.T) {
	svc, root := reloadService(t)
	writeCat(t, root, "agents/fresh-agent.yaml", "id: fresh-agent\nname: Fresh\n")
	writeCat(t, root, "launches/fresh-launch.yaml", "id: fresh-launch\nproject: demo\nagent: fresh-agent\nprovider: claude-code\nworkspace:\n  mode: worktree\n")

	if _, err := svc.Resolve("fresh-launch"); err == nil || !strings.Contains(err.Error(), "reload the catalog") {
		t.Fatalf("before reload: %v, want a refusal that says to reload", err)
	}

	report, err := svc.ReloadCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Reloaded || len(report.Added["agents"]) != 1 || report.Added["agents"][0] != "fresh-agent" || len(report.Added["launches"]) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if _, ok := svc.CurrentCatalog().Agents["fresh-agent"]; !ok {
		t.Fatal("the new agent is not in the live catalog")
	}
	if _, err := svc.Resolve("fresh-launch"); err != nil {
		t.Fatalf("after reload: %v", err)
	}

	// A change to an existing agent is reported as changed; removing one as removed.
	writeCat(t, root, "agents/fresh-agent.yaml", "id: fresh-agent\nname: Renamed\n")
	if err := os.Remove(filepath.Join(root, "launches", "fresh-launch.yaml")); err != nil {
		t.Fatal(err)
	}
	report, err = svc.ReloadCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Changed["agents"]) != 1 || len(report.Removed["launches"]) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if got := svc.CurrentCatalog().Agents["fresh-agent"].Name; got != "Renamed" {
		t.Fatalf("name = %q", got)
	}
}

// A catalog that does not load, or does not validate, changes nothing.
func TestReloadCatalogRefusesBrokenCatalogAndKeepsTheOld(t *testing.T) {
	svc, root := reloadService(t)
	before := svc.CurrentCatalog()

	writeCat(t, root, "agents/broken.yaml", "id: [this is: not valid yaml\n")
	if _, err := svc.ReloadCatalog(context.Background()); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("broken YAML: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "agents", "broken.yaml")); err != nil {
		t.Fatal(err)
	}

	writeCat(t, root, "launches/dangling.yaml", "id: dangling\nproject: demo\nagent: no-such-agent\nprovider: claude-code\n")
	if _, err := svc.ReloadCatalog(context.Background()); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("dangling reference: %v", err)
	}
	if svc.CurrentCatalog() != before {
		t.Fatal("a refused reload replaced the catalog")
	}
}

// global.yaml settings the daemon reads once at start are not swapped, and the
// report says a restart is needed.
func TestReloadCatalogKeepsRunningGlobalAndNamesTheRestart(t *testing.T) {
	svc, root := reloadService(t)
	oldGlobal := svc.CurrentCatalog().Global
	b, err := os.ReadFile(filepath.Join(root, "global.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	writeCat(t, root, "global.yaml", strings.Replace(string(b), "version: 0.1.0", "version: 9.9.9", 1))
	report, err := svc.ReloadCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RestartRequired) != 1 || !strings.Contains(report.RestartRequired[0], "global.yaml") {
		t.Fatalf("report = %+v", report)
	}
	if got := svc.CurrentCatalog().Global; got.Version != oldGlobal.Version {
		t.Fatalf("the running global.yaml settings were swapped: version %q", got.Version)
	}
}

// Readers and a reload at once: run under -race.
func TestReloadCatalogIsSafeAgainstConcurrentReaders(t *testing.T) {
	svc, root := reloadService(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					c := svc.CurrentCatalog()
					for range c.Agents {
					}
					_ = svc.ListAgents()
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		writeCat(t, root, "agents/churn.yaml", "id: churn\nname: v"+string(rune('a'+i))+"\n")
		if _, err := svc.ReloadCatalog(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
