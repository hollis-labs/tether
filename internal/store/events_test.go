package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
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

func TestListEventsBySession_NewestFirstWithPagination(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "evts.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Seed mixed sessions so we can verify filtering.
	for _, kind := range []string{"a", "b", "c", "d"} {
		if _, _, err := db.InsertEvent(events.ScopeSession, "s1", kind, ``); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := db.InsertEvent(events.ScopeSession, "s2", "other", ``); err != nil {
		t.Fatal(err)
	}

	// Limit 2, no cursor — returns newest 2 for s1.
	first, err := db.ListEventsBySession("s1", 2, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("got %d, want 2", len(first))
	}
	if first[0].Kind != "d" || first[1].Kind != "c" {
		t.Errorf("order wrong: %q, %q (want d,c)", first[0].Kind, first[1].Kind)
	}

	// Second page: cursor = smallest seq of first page.
	second, err := db.ListEventsBySession("s1", 2, first[1].Seq)
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("got %d, want 2", len(second))
	}
	if second[0].Kind != "b" || second[1].Kind != "a" {
		t.Errorf("page2 order wrong: %q, %q", second[0].Kind, second[1].Kind)
	}

	// All s1 rows + nothing else.
	all, err := db.ListEventsBySession("s1", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("all s1 returned %d; want 4", len(all))
	}

	rowsS2, err := db.ListEventsBySession("s2", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsS2) != 1 || rowsS2[0].Kind != "other" {
		t.Errorf("s2 rows = %+v", rowsS2)
	}
}

func TestQueryEvents_FiltersNewestFirst(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "query.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, _, err := db.InsertEvent(events.ScopeDaemon, "", "daemon.started", `{"pid":1}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.InsertEvent(events.ScopeSession, "s1", "session.state_changed", `{"to":"running"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.InsertEvent(events.ScopeBroker, "s1", "broker.envelope_sent", `{"id":"m1"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.InsertEvent(events.ScopeSession, "s2", "session.state_changed", `{"to":"stopped"}`); err != nil {
		t.Fatal(err)
	}

	got, err := db.QueryEvents(EventFilter{
		Scopes:    []events.Scope{events.ScopeSession, events.ScopeBroker},
		Kinds:     []string{"session.state_changed", "broker.envelope_sent"},
		SessionID: "s1",
		SinceSeq:  1,
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Kind != "broker.envelope_sent" || got[1].Kind != "session.state_changed" {
		t.Fatalf("order/filter wrong: %+v", got)
	}
	if got[0].SessionID != "s1" || got[1].SessionID != "s1" {
		t.Fatalf("session filter wrong: %+v", got)
	}
}

func TestQueryEvents_CursorPagination(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "query-cursor.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for _, kind := range []string{"a", "b", "c", "d"} {
		if _, _, err := db.InsertEvent(events.ScopeDaemon, "", kind, ``); err != nil {
			t.Fatal(err)
		}
	}

	first, err := db.QueryEvents(EventFilter{Limit: 2})
	if err != nil {
		t.Fatalf("QueryEvents first: %v", err)
	}
	if len(first) != 2 || first[0].Kind != "d" || first[1].Kind != "c" {
		t.Fatalf("first page = %+v", first)
	}

	second, err := db.QueryEvents(EventFilter{Limit: 2, Cursor: first[1].Seq})
	if err != nil {
		t.Fatalf("QueryEvents second: %v", err)
	}
	if len(second) != 2 || second[0].Kind != "b" || second[1].Kind != "a" {
		t.Fatalf("second page = %+v", second)
	}
}
