package consumers_test

import (
	"os"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

// TestMain has store.Open start new databases from one migrated image: under
// -race each Open otherwise replays every migration for ~4.5s.
func TestMain(m *testing.M) {
	store.UseMigratedTemplate()
	os.Exit(m.Run())
}
