package workerruntime_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// A finished session leaves its host directory behind with the shim gone. Restoring on the next worker
// start must skip it rather than exit: the first version returned the error, so one finished session
// put the worker into a crash loop and took it offline.
func TestRestoreSkipsDeadAndDamagedSessions(t *testing.T) {
	root := t.TempDir()
	hosts := filepath.Join(root, "hosts")
	for name, meta := range map[string]string{
		"damaged-meta": "{not json",
		"empty":        "",
	} {
		dir := filepath.Join(hosts, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if meta != "" {
			if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	e := workerruntime.NewExecutor(root, "unused", func(workerwire.Frame) error { return nil })
	if err := e.Restore(); err != nil {
		t.Fatalf("Restore must skip unusable session hosts, got %v", err)
	}
	if got := len(e.Inventory()); got != 0 {
		t.Fatalf("inventory = %d, want 0 (nothing restorable)", got)
	}
}
