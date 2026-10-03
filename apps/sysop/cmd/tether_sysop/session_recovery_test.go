package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

func TestCleanupPreservesRecoverySessions(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, state := range []string{"detached", "orphaned", "completed", "failed", "killed"} {
		if err := db.CreateSession(store.SessionRow{ID: state, State: state}, nil); err != nil {
			t.Fatal(err)
		}
		// Even malformed legacy rows with an ended_at are protected by state.
		if _, err := db.DB().Exec(`UPDATE sessions SET ended_at=? WHERE id=?`, "2000-01-01T00:00:00Z", state); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := cleanupCandidateSessionIDs(db.DB(), "2026-10-03T00:00:00Z", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"detached", "orphaned"} {
		if slices.Contains(ids, state) {
			t.Fatalf("cleanup includes non-terminal %s", state)
		}
	}
	for _, state := range []string{"completed", "failed", "killed"} {
		if !slices.Contains(ids, state) {
			t.Fatalf("cleanup lost terminal %s", state)
		}
	}
}
