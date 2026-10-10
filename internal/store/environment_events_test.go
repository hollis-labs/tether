package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
)

func TestEnvironmentWindowRetentionAndCursorDomains(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, session := range []string{"a", "b", "a"} {
		if _, _, err := db.InsertEvent(events.ScopeSession, session, "custom.kind", `{"value":1}`); err != nil {
			t.Fatal(err)
		}
	}
	w, err := db.EnvironmentEvents(ctx, 0, 10)
	if err != nil || w.GapReason != "" || w.HighWater != 3 || len(w.Events) != 3 {
		t.Fatalf("window %+v %v", w, err)
	}
	if w.Events[2].Seq != 3 {
		t.Fatal("cursor was renumbered")
	}
	w, err = db.EnvironmentEvents(ctx, 0, 2)
	if err != nil || w.GapReason != "too_large" || len(w.Events) != 0 {
		t.Fatalf("oversized %+v %v", w, err)
	}
	w, err = db.EnvironmentEvents(ctx, 4, 10)
	if err != nil || w.GapReason != "ahead" {
		t.Fatalf("ahead %+v %v", w, err)
	}
	if _, err = db.db.Exec(`DELETE FROM events WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	w, err = db.EnvironmentEvents(ctx, 0, 10)
	if err != nil || w.GapReason != "purged" || len(w.Events) != 0 {
		t.Fatalf("interior hole %+v %v", w, err)
	}
	if _, err = db.db.Exec(`DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	w, err = db.EnvironmentEvents(ctx, 0, 10)
	if err != nil || w.HighWater != 3 || w.EarliestAvailable != 4 || w.GapReason != "purged" {
		t.Fatalf("fully purged %+v %v", w, err)
	}
	w, err = db.EnvironmentEvents(ctx, 3, 10)
	if err != nil || w.GapReason != "" {
		t.Fatalf("snapshot cursor %+v %v", w, err)
	}
	seq, _, err := db.InsertEvent(events.ScopeDaemon, "", "daemon.started", "")
	if err != nil || seq != 4 {
		t.Fatalf("durable continuation seq=%d err=%v", seq, err)
	}
}
