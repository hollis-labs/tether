package launchresolve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/agentkit/agentlaunch"
)

// liveCatalogRoot returns the live ~/.tether/catalog path, or "" with a
// skip when it is not present (CI / clean machines).
func liveCatalogRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	root := filepath.Join(home, DefaultCatalogRelPath)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Skipf("live catalog %s not present", root)
	}
	return root
}

// TestLiveCatalog_Ingest exercises the registry against the real
// ~/.tether/catalog/ when it exists. It asserts only invariants that hold
// regardless of catalog size, so it is stable as the catalog evolves.
func TestLiveCatalog_Ingest(t *testing.T) {
	root := liveCatalogRoot(t)
	reg, err := OpenAt(Options{CatalogRoot: root})
	if err != nil {
		t.Fatalf("OpenAt live catalog: %v", err)
	}
	rep := reg.Report()

	if !rep.Reconciles() {
		t.Errorf("live ingest report does not reconcile")
	}
	if rep.Registered() == 0 {
		t.Error("live catalog registered zero records")
	}
	if len(rep.Errors) != 0 {
		t.Errorf("live ingest errors: %+v", rep.Errors)
	}
	// projects/ and sandbox-profiles/ have no registry kind: every skip
	// from them must be an unmapped-directory skip, never an invented kind.
	for _, s := range rep.Skipped {
		if s.Reason == agentlaunch.IngestSkipUnmappedDir &&
			s.Subdir != "projects" && s.Subdir != "sandbox-profiles" {
			t.Errorf("unexpected unmapped subdir in live catalog: %q", s.Subdir)
		}
	}

	t.Logf("live catalog %s: %d registered, by-kind=%v",
		root, rep.Registered(), rep.RegisteredByKind)
}
