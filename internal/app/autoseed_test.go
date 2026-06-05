package app

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

// TestMaybeAutoSeedCatalog_Empty verifies that an absent catalog root
// triggers the auto-seed: global.yaml is created and LoadLayered succeeds.
func TestMaybeAutoSeedCatalog_Empty(t *testing.T) {
	stateRoot := t.TempDir()
	catalogRoot := filepath.Join(stateRoot, "catalog")

	// Catalog dir doesn't exist yet.
	maybeAutoSeedCatalog(catalogRoot)

	// global.yaml must now exist.
	globalYAML := filepath.Join(catalogRoot, "global.yaml")
	if _, err := os.Stat(globalYAML); err != nil {
		t.Fatalf("global.yaml not created after auto-seed: %v", err)
	}

	// The written catalog must be loadable.
	cat, err := config.LoadLayered(catalogRoot)
	if err != nil {
		t.Fatalf("LoadLayered after auto-seed: %v", err)
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("Validate after auto-seed: %v", err)
	}
}

// TestMaybeAutoSeedCatalog_Existing verifies that an existing catalog is
// left untouched — auto-seed is a no-op when global.yaml is present.
func TestMaybeAutoSeedCatalog_Existing(t *testing.T) {
	stateRoot := t.TempDir()
	catalogRoot := filepath.Join(stateRoot, "catalog")
	if err := os.MkdirAll(catalogRoot, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write a sentinel global.yaml so the catalog appears to exist.
	sentinel := filepath.Join(catalogRoot, "global.yaml")
	content := []byte("# sentinel\nversion: 1.0.0\n")
	if err := os.WriteFile(sentinel, content, 0o640); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	maybeAutoSeedCatalog(catalogRoot)

	// File must be unchanged.
	got, err := os.ReadFile(sentinel) //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("read after no-op: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("existing global.yaml was modified by auto-seed")
	}
}

// TestMaybeAutoSeedCatalog_LogLine verifies the log message is emitted when
// auto-seeding occurs, and is NOT emitted when the catalog already exists.
func TestMaybeAutoSeedCatalog_LogLine(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	log.SetFlags(0) // suppress timestamp so we can match plainly

	stateRoot := t.TempDir()
	catalogRoot := filepath.Join(stateRoot, "catalog")

	maybeAutoSeedCatalog(catalogRoot)

	logged := buf.String()
	const want = "catalog absent"
	if !bytes.Contains([]byte(logged), []byte(want)) {
		t.Errorf("expected %q in log output, got: %q", want, logged)
	}

	// Second call — catalog now exists, no log line expected.
	buf.Reset()
	maybeAutoSeedCatalog(catalogRoot)
	if buf.Len() != 0 {
		t.Errorf("unexpected log output on second call: %q", buf.String())
	}
}

// TestNewAutoSeeds verifies that app.New succeeds when the catalog root
// doesn't exist yet (auto-seed fires and makes it bootable).
func TestNewAutoSeeds(t *testing.T) {
	stateRoot := t.TempDir()
	catalogRoot := filepath.Join(stateRoot, "catalog")

	// No catalog at all — New should auto-seed and succeed.
	svc, err := New(catalogRoot)
	if err != nil {
		t.Fatalf("New on empty catalog root: %v", err)
	}
	defer svc.Close()

	// Catalog must exist after New.
	if _, err := os.Stat(filepath.Join(catalogRoot, "global.yaml")); err != nil {
		t.Errorf("global.yaml not present after New: %v", err)
	}
}
