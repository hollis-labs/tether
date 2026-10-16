package environmentstream

import (
	"os"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

// TestMain has store.Open start new databases from one migrated image: under -race each Open otherwise replays every
// migration (~1.8s, more since the 0082 ALTERs), and these tests open many databases in-process.
func TestMain(m *testing.M) {
	store.UseMigratedTemplate()
	os.Exit(m.Run())
}
