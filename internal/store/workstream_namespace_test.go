package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// THE ACCEPTANCE CRITERION: a scratch note written in one session is readable
// from a session on the far side of a compaction, keyed only by the
// workstream. Asserted as namespace identity, since Tether stores no content
// and the namespace is the whole of what it contributes.
func TestSessionWorkstreamNamespace_SurvivesACompaction(t *testing.T) {
	db := openWorkstreamStore(t)

	ws, err := db.CreateWorkstream(WorkstreamRow{Name: "spans compaction"})
	if err != nil {
		t.Fatalf("CreateWorkstream: %v", err)
	}
	mustCreateSession(t, db, SessionRow{
		ID: "before", State: "running", Intent: "fresh",
		WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
	})
	// The compaction. The caller names no workstream — S1 inherits it.
	mustCreateSession(t, db, SessionRow{
		ID: "after", State: "running", Intent: "compact", ParentSessionID: parentRef("before"),
	})

	nsBefore, err := db.SessionWorkstreamNamespace("chrispian", "before", "notes")
	if err != nil {
		t.Fatalf("namespace(before): %v", err)
	}
	nsAfter, err := db.SessionWorkstreamNamespace("chrispian", "after", "notes")
	if err != nil {
		t.Fatalf("namespace(after): %v", err)
	}
	if nsBefore != nsAfter {
		t.Errorf("containment did not survive the compaction:\n  before %s\n  after  %s", nsBefore, nsAfter)
	}
	want := "user/chrispian/session/ws_" + ws.ID + "/memory/notes"
	if nsBefore != want {
		t.Errorf("namespace = %q, want %q", nsBefore, want)
	}
}

// The prefix is the whole reason a workstream id is tolerable in a {sid} slot:
// a bare id fails silently against sessions.id, a prefixed one fails visibly.
func TestSessionWorkstreamNamespace_IDIsPrefixed(t *testing.T) {
	db := openWorkstreamStore(t)
	ws, _ := db.CreateWorkstream(WorkstreamRow{})
	mustCreateSession(t, db, SessionRow{
		ID: "s", State: "running", Intent: "fresh",
		WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
	})

	got, err := db.SessionWorkstreamNamespace("chrispian", "s", "todos")
	if err != nil {
		t.Fatalf("namespace: %v", err)
	}
	if !strings.Contains(got, "/session/ws_") {
		t.Errorf("namespace = %q; the workstream id must be prefixed so it cannot be mistaken for a session id", got)
	}
	if strings.Contains(got, "/session/"+ws.ID) {
		t.Errorf("namespace = %q carries a BARE workstream id in the session slot; that is the silent-failure shape", got)
	}
}

// Handing a workstream id where a session id belongs must not quietly produce
// ws_<session-id> — a well-formed namespace containing a lie, and the one
// input where the prefix makes things worse rather than better.
func TestWorkstreamNamespace_RefusesASessionID(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "sess-x", State: "running", Intent: "fresh"})

	_, err := db.workstreamNamespace("chrispian", "sess-x", "notes")
	if !errors.Is(err, ErrNotAWorkstream) {
		t.Fatalf("err = %v, want ErrNotAWorkstream", err)
	}
	if !strings.Contains(err.Error(), "is a session id") {
		t.Errorf("error should name the likely mistake, got: %v", err)
	}
}

func TestWorkstreamNamespace_RefusesAnUnknownID(t *testing.T) {
	db := openWorkstreamStore(t)
	if _, err := db.workstreamNamespace("chrispian", "nothing-here", "notes"); !errors.Is(err, ErrNotAWorkstream) {
		t.Errorf("err = %v, want ErrNotAWorkstream", err)
	}
}

// A namespace lookup must not silently mutate the session's lineage. Creating
// a workstream is EnsureSessionWorkstream's job and is one explicit call away;
// doing it here would make a read that writes.
func TestSessionWorkstreamNamespace_DoesNotCreateAWorkstream(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "solo", State: "running", Intent: "fresh"})

	if _, err := db.SessionWorkstreamNamespace("chrispian", "solo", "notes"); err == nil {
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

// The memory type is Tesseract's vocabulary and is passed through. Mirroring
// the list here would create a second copy that drifts — Tether would start
// rejecting a type Tesseract had just added — so an unknown type is Tesseract's
// error to raise, not ours.
func TestSessionWorkstreamNamespace_DoesNotValidateMemoryType(t *testing.T) {
	db := openWorkstreamStore(t)
	ws, _ := db.CreateWorkstream(WorkstreamRow{})
	mustCreateSession(t, db, SessionRow{
		ID: "s", State: "running", Intent: "fresh",
		WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
	})

	got, err := db.SessionWorkstreamNamespace("chrispian", "s", "a_type_tether_has_never_heard_of")
	if err != nil {
		t.Fatalf("an unrecognized type must pass through, not be rejected here: %v", err)
	}
	if !strings.HasSuffix(got, "/memory/a_type_tether_has_never_heard_of") {
		t.Errorf("namespace = %q", got)
	}
}

func TestSessionWorkstreamNamespace_RequiresItsArguments(t *testing.T) {
	db := openWorkstreamStore(t)
	for _, c := range []struct{ user, session, mtype string }{
		{"", "s", "notes"},
		{"chrispian", "", "notes"},
		{"chrispian", "s", ""},
	} {
		if _, err := db.SessionWorkstreamNamespace(c.user, c.session, c.mtype); err == nil {
			t.Errorf("(%q,%q,%q) was accepted", c.user, c.session, c.mtype)
		}
	}
}
