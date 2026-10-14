package federation

import (
	"fmt"
	"os"
	"testing"

	"github.com/hollis-labs/tether/internal/testutil"
)

// TestMain canonicalizes TMPDIR so t.TempDir paths survive the production
// symlink checks on macOS, where /var is a symlink to /private/var.
func TestMain(m *testing.M) {
	if err := testutil.CanonicalizeTempEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "canonical temp dir:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
