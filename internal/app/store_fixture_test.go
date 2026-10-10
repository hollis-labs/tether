package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

var appFixtureSchema struct {
	once sync.Once
	data []byte
	err  error
}

// openAppFixtureStore shares only the immutable bytes of an empty, fully
// migrated database. Every fixture still owns a separate file and connection,
// reopened through store.Open to exercise its normal configuration and fences.
// Migration upgrade tests continue to use store.Open directly.
func openAppFixtureStore(t *testing.T, path string) (*store.Store, error) {
	t.Helper()
	appFixtureSchema.once.Do(func() {
		seed := filepath.Join(t.TempDir(), "schema.db")
		db, err := store.Open(seed)
		if err != nil {
			appFixtureSchema.err = err
			return
		}
		// Closing the sole connection checkpoints the WAL before reading the
		// main file. Retain bytes, not the path owned by the first test.
		if err := db.Close(); err != nil {
			appFixtureSchema.err = err
			return
		}
		appFixtureSchema.data, appFixtureSchema.err = os.ReadFile(seed)
	})
	if appFixtureSchema.err != nil {
		return nil, fmt.Errorf("prepare app fixture schema: %w", appFixtureSchema.err)
	}
	if err := os.WriteFile(path, appFixtureSchema.data, 0600); err != nil {
		return nil, fmt.Errorf("copy app fixture schema: %w", err)
	}
	return store.Open(path)
}

func TestAppFixtureStoresRemainIndependent(t *testing.T) {
	firstPath := filepath.Join(t.TempDir(), "first.db")
	first, err := openAppFixtureStore(t, firstPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := openAppFixtureStore(t, filepath.Join(t.TempDir(), "second.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	for _, db := range []*store.Store{first, second} {
		if err := db.CreateSession(store.SessionRow{ID: "same-session", State: "created"}, nil); err != nil {
			t.Fatal("fixtures shared session state", err)
		}
	}
	if err := first.UpdateSessionState("same-session", "completed", 0, nil); err != nil {
		t.Fatal(err)
	}
	row, err := second.GetSession("same-session")
	if err != nil || row.State != "created" {
		t.Fatal("terminal mutation crossed fixture boundary", row, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	row, err = reopened.GetSession("same-session")
	if err != nil || row.State != "completed" {
		t.Fatal("reopen lost private fixture state", row, err)
	}
}
