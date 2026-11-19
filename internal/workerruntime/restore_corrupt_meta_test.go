package workerruntime

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/workerwire"
)

// @photon-review on #557: with meta.json unreadable the host pid cannot be checked, so a dir past the grace
// age would be moved even if its shim-host were alive (taking its control socket with it). Such a dir is
// counted failed and left in place when a shim-host serves its launch descriptor; with none, it is dead.
func TestRestoreKeepsACorruptRecordWithALiveHost(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * DeadHostGrace)
	for _, name := range []string{"served", "unserved"} {
		dir := filepath.Join(root, "hosts", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"id":`), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(filepath.Join(dir, "meta.json"), old, old)
		_ = os.Chtimes(dir, old, old)
	}
	served := filepath.Join(root, "hosts", "served", "launch.json")
	defer func(orig func() []string) { processArgs = orig }(processArgs)
	processArgs = func() []string {
		return []string{"/usr/bin/sleep 1", "/opt/tether-worker shim-host --launch " + served + " --x", ""}
	}
	e := NewExecutor(root, "unused", func(workerwire.Frame) error { return nil })
	if err := e.Restore(); err != nil {
		t.Fatal(err)
	}
	want := workerwire.RestoreCounts{DeadSkipped: 1, Pruned: 1, Failed: 1}
	if got := e.RestoreCounts(); got == nil || *got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, "hosts", "served")); err != nil {
		t.Fatalf("a dir with a live host was moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "hosts", "unserved")); !os.IsNotExist(err) {
		t.Fatalf("the dead dir was not pruned: %v", err)
	}
}
