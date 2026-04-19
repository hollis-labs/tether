package store

import (
	"path/filepath"
	"testing"

	"github.com/chrispian/agent-mux/internal/events"
)

func TestLogEvent_SessionAndDaemonScopes(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ev.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.LogEvent(events.ScopeSession, "sess-1", "state", `{"to":"running"}`); err != nil {
		t.Fatalf("log session event: %v", err)
	}
	if err := db.LogEvent(events.ScopeDaemon, "", "startup", ""); err != nil {
		t.Fatalf("log daemon event: %v", err)
	}

	// Inspect the table directly; a public events query API lives in
	// Sprint v002-06. This test is about persistence shape only.
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
	// Daemon event: session_id must be SQL NULL + payload must be SQL NULL.
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

func TestLogEvent_RejectsEmptyScope(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "evr.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.LogEvent("", "sess", "kind", ""); err == nil {
		t.Fatal("expected error for empty scope, got nil")
	}
}
