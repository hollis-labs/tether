package sshenroll

import (
	"fmt"
	"os"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/testutil"
)

// TestMain canonicalizes TMPDIR so t.TempDir paths survive the production
// symlink checks on macOS, where /var is a symlink to /private/var, and starts new databases from one migrated image
// (store.UseMigratedTemplate) instead of replaying every migration per Open.
func TestMain(m *testing.M) {
	if err := testutil.CanonicalizeTempEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "canonical temp dir:", err)
		os.Exit(1)
	}
	store.UseMigratedTemplate()
	os.Exit(m.Run())
}
