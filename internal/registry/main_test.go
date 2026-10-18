package registry_test

import (
	"os"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

// TestMain has store.Open start new databases from one migrated image. Under
// -race, replaying every migration on a fresh database costs ~4.5s, and most
// tests here need a migrated database rather than the migration itself.
func TestMain(m *testing.M) {
	store.UseMigratedTemplate()
	os.Exit(m.Run())
}
