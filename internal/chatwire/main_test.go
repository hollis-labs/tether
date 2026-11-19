package chatwire

import (
	"os"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

// TestMain has store.Open start new databases from one migrated image: every case here opens its own Tether store.
func TestMain(m *testing.M) {
	store.UseMigratedTemplate()
	os.Exit(m.Run())
}
