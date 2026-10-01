package app

import (
	"os"
	"path/filepath"
	"testing"
)

// writeStateCatalog writes a minimal catalog whose state_db sits in stateDir.
func writeStateCatalog(t *testing.T, stateDir string) string {
	t.Helper()
	root := t.TempDir()
	global := "version: 1.0.0\ncatalog:\n  defaults:\n    state_db: " + filepath.Join(stateDir, "tether.db") + "\n"
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// The daemon-only `mux mcp` runs where the state directory is read-only
// (CW-20261001-0173). NewCatalogOnly must load the catalog and touch nothing
// under it: no database, no WAL, no migration.
func TestNewCatalogOnly_NeverTouchesTheStateDirectory(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0o500); err != nil { // read-only, like a protected path
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })
	root := writeStateCatalog(t, stateDir)

	svc, err := NewCatalogOnly(root)
	if err != nil {
		t.Fatalf("NewCatalogOnly: %v", err)
	}
	if svc.Store != nil || svc.Manager != nil || svc.Bus != nil || svc.Registry != nil {
		t.Errorf("a catalog-only Service holds state: Store=%v Manager=%v Bus=%v Registry=%v", svc.Store, svc.Manager, svc.Bus, svc.Registry)
	}
	if svc.Catalog == nil || svc.CatalogRoot != root {
		t.Errorf("catalog not loaded: %+v", svc)
	}
	if err := svc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if entries, err := os.ReadDir(stateDir); err != nil || len(entries) != 0 {
		t.Errorf("state directory = %v (err %v), want empty: a catalog-only Service must not create anything there", entries, err)
	}
}

// NewCatalogOnly does not seed a missing catalog: the catalog is the
// daemon's to write.
func TestNewCatalogOnly_DoesNotSeedAMissingCatalog(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	_, _ = NewCatalogOnly(root) // whether an absent catalog loads is not the point
	if _, err := os.Stat(filepath.Join(root, "global.yaml")); err == nil {
		t.Errorf("NewCatalogOnly seeded a catalog at %s", root)
	}
}
