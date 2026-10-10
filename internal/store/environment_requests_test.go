package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
)

func TestEnvironmentRequestsExactResolutionRetentionAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.CreateSession(SessionRow{ID: "s", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	publish := func(turn, id, kind string, open bool, sequence uint64) {
		t.Helper()
		raw, _ := json.Marshal(EnvironmentRequest{TurnID: turn, RequestID: id, Kind: kind, Open: open, SourceSequence: sequence})
		if _, _, err := db.InsertEvent(events.ScopeSession, "s", EnvironmentRequestKind, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	publish("turn", "one", "question", true, 1)
	before, err := db.EnvironmentSnapshot(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	publish("turn", "two", "approval", true, 2)
	publish("other-turn", "one", "", false, 3)
	publish("turn", "one", "", false, 4)
	publish("turn", "one", "question", true, 1) // stale replay cannot reopen
	after, err := db.EnvironmentSnapshot(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Requests) != 1 || !before.Requests[0].Open || before.HighWater != 1 {
		t.Fatalf("snapshot leaked newer status %+v", before)
	}
	if len(after.Requests) != 2 || after.Requests[0].Open || !after.Requests[1].Open {
		t.Fatalf("matching/concurrent state %+v", after)
	}
	if _, err = db.DB().Exec(`DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := db.EnvironmentSnapshot(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Requests) != 2 || restarted.Requests[0].Open || !restarted.Requests[1].Open || restarted.HighWater != 5 {
		t.Fatalf("retention/restart lost state %+v", restarted)
	}
}
