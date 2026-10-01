//go:build linux

package mcpadapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

// The review's reproduction through the real handlers: a project-scope
// mux_agent_create, and a mux_agent_edit, while the repository's .tether is
// flipped between two symlinks, one into the protected catalog. Checking a
// resolved path and writing it later lost this race (3861 of 20000 creates wrote
// into the catalog); the handlers write through the directory they judged, so
// the catalog is never written, whatever an agent's shell does to the symlink.
func TestAgentOps_SymlinkFlipNeverWritesTheProtectedCatalog(t *testing.T) {
	iterations := 1500
	if testing.Short() {
		iterations = 300
	}
	t.Setenv("HOME", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalogRoot, repo, safe := filepath.Join(base, "catalog"), filepath.Join(base, "repo"), filepath.Join(base, "safe")
	const victim = "id: victim\nname: V\nsystem_prompt: operator-authored\n"
	for _, d := range []string{filepath.Join(catalogRoot, "agents"), repo, filepath.Join(safe, "agents")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{catalogRoot, safe} {
		if err := os.WriteFile(filepath.Join(d, "agents", "victim.yaml"), []byte(victim), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(repo) // project-layer discovery is anchored at the working directory

	a := newAgentOpsAdapter(t, catalogRoot, map[string]config.Project{"p": {RepoRoot: repo}}, ScopeCatalogWrite)
	a.SetProtectedPaths([]string{catalogRoot})

	link := filepath.Join(repo, ".tether")
	flip := func(target string) {
		tmp := link + ".new"
		_ = os.Remove(tmp)
		if err := os.Symlink(target, tmp); err == nil {
			_ = os.Rename(tmp, link)
		}
	}
	flip(safe)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				flip(catalogRoot)
			} else {
				flip(safe)
			}
		}
	}()

	var created, edited int
	for i := 0; i < iterations; i++ {
		if _, err := a.handleAgentCreate(context.Background(), map[string]any{"id": fmt.Sprintf("race-%d", i), "scope": "project", "project": "p", "name": "R"}); err == nil {
			created++
		}
		if _, err := a.handleAgentEdit(context.Background(), map[string]any{"id": "victim", "system_prompt": fmt.Sprintf("injected-%d", i)}); err == nil {
			edited++
		}
	}
	close(stop)
	wg.Wait()

	entries, err := os.ReadDir(filepath.Join(catalogRoot, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "victim.yaml" {
			t.Fatalf("a create wrote %s into the protected catalog (%d of %d creates landed outside it)", e.Name(), created, iterations)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(catalogRoot, "agents", "victim.yaml")); string(got) != victim { //nolint:gosec // test-owned path
		t.Fatalf("an edit rewrote the protected catalog's agent:\n%s", got)
	}
	t.Logf("%d iterations: %d creates and %d edits landed outside the catalog, none inside", iterations, created, edited)
	if created == 0 || edited == 0 {
		t.Fatalf("created = %d, edited = %d: the test did not exercise the flip", created, edited)
	}
}
