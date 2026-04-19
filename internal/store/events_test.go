package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/events"
)

func TestInsertEvent_SessionAndDaemonScopes(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ev.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	seq1, at1, err := db.InsertEvent(events.ScopeSession, "sess-1", "state", `{"to":"running"}`)
	if err != nil {
		t.Fatalf("insert session event: %v", err)
	}
	if seq1 != 1 {
		t.Errorf("seq1 = %d, want 1", seq1)
	}
	if at1.IsZero() {
		t.Error("at1 is zero")
	}

	seq2, _, err := db.InsertEvent(events.ScopeDaemon, "", "startup", "")
	if err != nil {
		t.Fatalf("insert daemon event: %v", err)
	}
	if seq2 != 2 {
		t.Errorf("seq2 = %d, want 2", seq2)
	}

	// Inspect the table directly to verify NULL handling.
	rows, err := db.db.Query(`SELECT scope, session_id, kind, payload_json FROM events ORDER BY id`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()

	type got struct {
		scope, sid, kind, payload *string
	}
	var out []got
	for rows.Next() {
		var scope, sid, kind, payload *string
		if err := rows.Scan(&scope, &sid, &kind, &payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, got{scope, sid, kind, payload})
	}
	if len(out) != 2 {
		t.Fatalf("rows = %d, want 2", len(out))
	}
	if out[0].scope == nil || *out[0].scope != "session" {
		t.Errorf("row 0 scope = %v, want 'session'", out[0].scope)
	}
	if out[0].sid == nil || *out[0].sid != "sess-1" {
		t.Errorf("row 0 session_id = %v, want 'sess-1'", out[0].sid)
	}
	if out[1].scope == nil || *out[1].scope != "daemon" {
		t.Errorf("row 1 scope = %v, want 'daemon'", out[1].scope)
	}
	if out[1].sid != nil {
		t.Errorf("row 1 session_id = %v, want NULL", *out[1].sid)
	}
	if out[1].payload != nil {
		t.Errorf("row 1 payload_json = %v, want NULL", *out[1].payload)
	}
}

func TestInsertEvent_RejectsEmptyScope(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "evr.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, _, err := db.InsertEvent("", "sess", "kind", ""); err == nil {
		t.Fatal("expected error for empty scope, got nil")
	}
}

func TestEventsSince_ReturnsEventsAfterSeqInOrder(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "since.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for i := 0; i < 5; i++ {
		if _, _, err := db.InsertEvent(events.ScopeSession, "s1", "k", `{}`); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.EventsSince(2)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, wantSeq := range []int64{3, 4, 5} {
		if got[i].Seq != wantSeq {
			t.Errorf("got[%d].Seq = %d, want %d", i, got[i].Seq, wantSeq)
		}
		if got[i].SessionID != "s1" {
			t.Errorf("got[%d].SessionID = %q, want s1", i, got[i].SessionID)
		}
		if got[i].At.IsZero() {
			t.Errorf("got[%d].At is zero", i)
		}
	}
}

func TestEventsSince_ZeroReturnsAll(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "all.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for i := 0; i < 3; i++ {
		if _, _, err := db.InsertEvent(events.ScopeDaemon, "", "k", ""); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.EventsSince(0)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
}

func TestEventsSince_AtRoundTrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "rt.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	before := time.Now().UTC().Add(-time.Second)
	_, at, err := db.InsertEvent(events.ScopeSession, "s1", "k", ``)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC().Add(time.Second)

	got, err := db.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	// Event.At parsed from store must match InsertEvent's returned at
	// to at least second precision (RFC3339Nano round-trip).
	if got[0].At.Before(before) || got[0].At.After(after) {
		t.Errorf("got.At = %v, want between %v and %v", got[0].At, before, after)
	}
	// Sanity: the returned at and stored at agree.
	if !got[0].At.Equal(at) {
		t.Errorf("stored at %v != returned at %v", got[0].At, at)
	}
}
