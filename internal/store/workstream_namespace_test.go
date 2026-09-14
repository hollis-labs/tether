package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// The primary property of the container: a compaction creates a new session
// row, and anything keyed on a session id is orphaned by it. A note written
// before a compaction must be placed where a session after the compaction finds
// it, using only the workstream.
func TestSessionWorkstreamNamespace_SurvivesACompaction(t *testing.T) {
	db := openWorkstreamStore(t)
	ws, err := db.CreateWorkstream(WorkstreamRow{Name: "feature"})
	if err != nil {
		t.Fatalf("CreateWorkstream: %v", err)
	}

	mustCreateSession(t, db, SessionRow{
		ID: "before", State: "ended", Intent: "fresh", ProjectID: "PRJ-TEST",
		WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
	})
	mustCreateSession(t, db, SessionRow{
		ID: "after", State: "running", Intent: "compact", ProjectID: "PRJ-TEST",
		ParentSessionID: sql.NullString{String: "before", Valid: true},
	})

	nsBefore, err := db.SessionWorkstreamNamespace("before")
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	nsAfter, err := db.SessionWorkstreamNamespace("after")
	if err != nil {
		t.Fatalf("after: %v", err)
	}

	if nsBefore.Namespace != nsAfter.Namespace {
		t.Errorf("namespace changed across compaction:\n  before: %s\n  after:  %s",
			nsBefore.Namespace, nsAfter.Namespace)
	}
	if nsBefore.WorkstreamID != nsAfter.WorkstreamID || nsBefore.WorkstreamID != ws.ID {
		t.Errorf("workstream id mismatch: before=%q, after=%q, ws=%q",
			nsBefore.WorkstreamID, nsAfter.WorkstreamID, ws.ID)
	}
	want := "project/PRJ-TEST/workspace/scratch"
	if nsBefore.Namespace != want {
		t.Errorf("namespace = %q, want %q", nsBefore.Namespace, want)
	}
}

// Chrispian's ruling (2026-09-13, CW-20260912-0062): workstream_id is an attribute,
// never a namespace path partition. The ws_<id> prefix and pseudo-session segment
// are retired. Target is workspace domain.
func TestSessionWorkstreamNamespace_WorkstreamIDIsAnAttributeNotPathSegment(t *testing.T) {
	db := openWorkstreamStore(t)
	ws, _ := db.CreateWorkstream(WorkstreamRow{})
	mustCreateSession(t, db, SessionRow{
		ID: "s", State: "running", Intent: "fresh", ProjectID: "tether",
		WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
	})

	got, err := db.SessionWorkstreamNamespace("s")
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}

	// Must NOT contain ws_ or the workstream ID in the path
	if strings.Contains(got.Namespace, "ws_") {
		t.Errorf("namespace = %q contains retired ws_ prefix", got.Namespace)
	}
	if strings.Contains(got.Namespace, ws.ID) {
		t.Errorf("namespace = %q contains workstream ID in path; workstream must be an attribute", got.Namespace)
	}
	if strings.Contains(got.Namespace, "memory/notes") {
		t.Errorf("namespace = %q targets memory domain; workspace domain required", got.Namespace)
	}
	wantNamespace := "project/tether/workspace/scratch"
	if got.Namespace != wantNamespace {
		t.Errorf("namespace = %q, want %q", got.Namespace, wantNamespace)
	}
	if got.WorkstreamID != ws.ID {
		t.Errorf("WorkstreamID = %q, want %q", got.WorkstreamID, ws.ID)
	}
}

// Handing a session id where a workstream id is required must be refused.
func TestWorkstreamTarget_RefusesASessionID(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "sess-x", State: "running", Intent: "fresh", ProjectID: "p1"})

	_, err := db.WorkstreamTarget("sess-x", WorkstreamTargetOptions{Project: "p1"})
	if !errors.Is(err, ErrNotAWorkstream) {
		t.Fatalf("err = %v, want ErrNotAWorkstream", err)
	}
	if !strings.Contains(err.Error(), "is a session id") {
		t.Errorf("error should name the likely mistake, got: %v", err)
	}
}

func TestWorkstreamTarget_RefusesAnUnknownID(t *testing.T) {
	db := openWorkstreamStore(t)
	if _, err := db.WorkstreamTarget("nothing-here", WorkstreamTargetOptions{Project: "p1"}); !errors.Is(err, ErrNotAWorkstream) {
		t.Errorf("err = %v, want ErrNotAWorkstream", err)
	}
}

// A namespace lookup must not silently mutate the session's lineage. Creating
// a workstream is EnsureSessionWorkstream's job and is one explicit call away;
// doing it here would make a read that writes.
func TestSessionWorkstreamNamespace_DoesNotCreateAWorkstream(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "solo", State: "running", Intent: "fresh", ProjectID: "PRJ-1"})

	if _, err := db.SessionWorkstreamNamespace("solo"); err == nil {
		t.Fatal("expected an error for a session with no workstream")
	}
	got, err := db.GetSession("solo")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.WorkstreamID.Valid {
		t.Errorf("the namespace lookup created a workstream (%q); it must be a read", got.WorkstreamID.String)
	}
	list, err := db.ListWorkstreams(ListWorkstreamsOptions{})
	if err != nil {
		t.Fatalf("ListWorkstreams: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a workstream was created as a side effect: %+v", list)
	}
}

// An ownerless session (no ProjectID on session row) must be refused when
// no explicit project or owner is supplied in options.
func TestSessionWorkstreamNamespace_OwnerlessSessionRefused(t *testing.T) {
	db := openWorkstreamStore(t)
	ws, _ := db.CreateWorkstream(WorkstreamRow{})
	mustCreateSession(t, db, SessionRow{
		ID: "ownerless", State: "running", Intent: "fresh",
		WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
	})

	_, err := db.SessionWorkstreamNamespace("ownerless")
	if err == nil {
		t.Fatal("expected error for ownerless session with no explicit project/owner")
	}
	if !strings.Contains(err.Error(), "session has no declared project") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// Options allow explicit project, owner (e.g. app/tether), or custom tail.
func TestSessionWorkstreamNamespace_PlacementOverrides(t *testing.T) {
	db := openWorkstreamStore(t)
	ws, _ := db.CreateWorkstream(WorkstreamRow{})
	mustCreateSession(t, db, SessionRow{
		ID: "s1", State: "running", Intent: "fresh", ProjectID: "default-proj",
		WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
	})

	// 1. Explicit project override
	resProj, err := db.SessionWorkstreamNamespace("s1", WorkstreamTargetOptions{Project: "override-proj"})
	if err != nil {
		t.Fatalf("project override: %v", err)
	}
	if resProj.Namespace != "project/override-proj/workspace/scratch" {
		t.Errorf("got %q, want project/override-proj/workspace/scratch", resProj.Namespace)
	}

	// 2. Explicit app owner override (Tether's cross-project scratch)
	resApp, err := db.SessionWorkstreamNamespace("s1", WorkstreamTargetOptions{Owner: "app/tether"})
	if err != nil {
		t.Fatalf("owner override: %v", err)
	}
	if resApp.Namespace != "app/tether/workspace/scratch" {
		t.Errorf("got %q, want app/tether/workspace/scratch", resApp.Namespace)
	}

	// 3. Custom tail
	resTail, err := db.SessionWorkstreamNamespace("s1", WorkstreamTargetOptions{Tail: "notes"})
	if err != nil {
		t.Fatalf("tail override: %v", err)
	}
	if resTail.Namespace != "project/default-proj/workspace/notes" {
		t.Errorf("got %q, want project/default-proj/workspace/notes", resTail.Namespace)
	}
}

func TestSessionWorkstreamNamespace_RequiresSessionID(t *testing.T) {
	db := openWorkstreamStore(t)
	if _, err := db.SessionWorkstreamNamespace(""); err == nil {
		t.Error("expected error for empty sessionID")
	}
}
