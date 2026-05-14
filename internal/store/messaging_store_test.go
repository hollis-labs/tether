package store_test

import (
	"path/filepath"
	"testing"

	"github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/messagingtest"

	"github.com/hollis-labs/tether/internal/store"
)

// TestMessagingStore_Contract runs the full go-messaging contract suite
// against the SQLite-backed Store implementation.
func TestMessagingStore_Contract(t *testing.T) {
	messagingtest.RunContract(t, func(t *testing.T) messaging.Store {
		db, err := store.Open(filepath.Join(t.TempDir(), "msg.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return db.MessagingStore()
	})
}
