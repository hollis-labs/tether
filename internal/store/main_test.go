package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/testutil"
)

// TestMain canonicalizes TMPDIR so t.TempDir paths survive the production
// symlink checks on macOS, where /var is a symlink to /private/var.
//
// It also builds one migrated empty database and has Open start new databases
// from that image (openTemplate). Replaying every migration costs ~1.8s per Open
// under -race and these tests open hundreds of databases, which made the store
// race job take 15-20 minutes against a 20 minute limit.
func TestMain(m *testing.M) {
	if err := testutil.CanonicalizeTempEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "canonical temp dir:", err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "store-template-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "template dir:", err)
		os.Exit(1)
	}
	path := filepath.Join(dir, "template.db")
	s, err := Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "template database:", err)
		os.Exit(1)
	}
	// Closing the last connection checkpoints the WAL into the main file.
	if err := s.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "template close:", err)
		os.Exit(1)
	}
	image, err := os.ReadFile(path) //nolint:gosec // G304: the file this function just created
	if err != nil {
		fmt.Fprintln(os.Stderr, "template read:", err)
		os.Exit(1)
	}
	_ = os.RemoveAll(dir)
	openTemplate = image
	os.Exit(m.Run())
}
