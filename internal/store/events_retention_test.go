package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// insertEventAt writes an events row stamped at, as InsertEvent would have
// at that time.
func insertEventAt(t *testing.T, s *Store, at time.Time) int64 {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO events (scope, session_id, at, kind, payload_json) VALUES ('session', 's1', ?, 'session.state_changed', NULL)`,
		at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func eventCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// DeleteEventsBefore removes only rows older than the cutoff, oldest first,
// at most limit per call (CW-20260930-0008).
func TestDeleteEventsBefore_BoundedAndOldestFirst(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	cutoff := now.Add(-90 * 24 * time.Hour)

	if n, err := s.DeleteEventsBefore(ctx, cutoff, 10); err != nil || n != 0 {
		t.Fatalf("empty table: deleted %d, %v; want 0, nil", n, err)
	}
	for i := 0; i < 3; i++ {
		insertEventAt(t, s, now.Add(-100*24*time.Hour+time.Duration(i)*time.Minute))
	}
	keep := []int64{insertEventAt(t, s, now.Add(-10*24*time.Hour)), insertEventAt(t, s, now)}

	if n, err := s.DeleteEventsBefore(ctx, cutoff, 2); err != nil || n != 2 {
		t.Fatalf("first batch: deleted %d, %v; want 2", n, err)
	}
	if n, err := s.DeleteEventsBefore(ctx, cutoff, 2); err != nil || n != 1 {
		t.Fatalf("second batch: deleted %d, %v; want the last old row", n, err)
	}
	if n, err := s.DeleteEventsBefore(ctx, cutoff, 2); err != nil || n != 0 {
		t.Fatalf("nothing left: deleted %d, %v; want 0", n, err)
	}
	evs, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Seq != keep[0] || evs[1].Seq != keep[1] {
		t.Fatalf("remaining = %+v; want the two rows inside the window", evs)
	}
	if _, err := s.DeleteEventsBefore(ctx, cutoff, 0); err == nil {
		t.Fatal("limit 0 accepted; want an error")
	}
}
