package store

import (
	"fmt"
	"os"
	"testing"

	"github.com/hollis-labs/tether/internal/testutil"
)

// TestMain canonicalizes TMPDIR so t.TempDir paths survive the production
// symlink checks on macOS, where /var is a symlink to /private/var.
//
// It also has Open start new databases from one migrated image
// (UseMigratedTemplate): replaying every migration costs ~1.8s per Open under
// -race and these tests open hundreds of databases, which made the store race
// job take 15-20 minutes against a 20 minute limit.
func TestMain(m *testing.M) {
	if err := testutil.CanonicalizeTempEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "canonical temp dir:", err)
		os.Exit(1)
	}
	UseMigratedTemplate()
	os.Exit(m.Run())
}
