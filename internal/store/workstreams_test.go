package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func openWorkstreamStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustCreateSession(t *testing.T, db *Store, row SessionRow) SessionRow {
	t.Helper()
	if err := db.CreateSession(row, &launch.Plan{}); err != nil {
		t.Fatalf("CreateSession(%q): %v", row.ID, err)
	}
	got, err := db.GetSession(row.ID)
	if err != nil {
		t.Fatalf("GetSession(%q): %v", row.ID, err)
	}
	return *got
}

func parentRef(id string) sql.NullString {
	return sql.NullString{String: id, Valid: true}
}

// This is S1's acceptance criterion, and the one it is easy to get wrong by
// implementing for 'compact' alone and calling it done. A compaction creates a
// NEW session row, so a container keyed on session_id is orphaned by exactly
// the event it exists to survive — and 'resume' and 'fork' create new rows the
// same way. All three are tested directly rather than by inspection.
func TestCreateSession_InheritsWorkstreamAcrossEveryLineageIntent(t *testing.T) {
	for _, intent := range []string{"resume", "compact", "fork"} {
		t.Run(intent, func(t *testing.T) {
			db := openWorkstreamStore(t)

			ws, err := db.CreateWorkstream(WorkstreamRow{Name: "sprint work"})
			if err != nil {
				t.Fatalf("CreateWorkstream: %v", err)
			}

			parent := mustCreateSession(t, db, SessionRow{
				ID:           "parent",
				State:        "running",
				Intent:       "fresh",
				WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
			})
			if parent.WorkstreamID.String != ws.ID {
				t.Fatalf("parent workstream = %q, want %q", parent.WorkstreamID.String, ws.ID)
			}

			// The caller says nothing about a workstream — that is the point.
			child := mustCreateSession(t, db, SessionRow{
				ID:              "child",
				State:           "running",
				Intent:          intent,
				ParentSessionID: parentRef("parent"),
			})
			if child.WorkstreamID.String != ws.ID {
				t.Errorf("%s child workstream = %q, want inherited %q", intent, child.WorkstreamID.String, ws.ID)
			}
		})
	}
}

// A chain is what actually happens in life: fresh -> compact -> resume. The
// container has to survive the whole way, not just one hop.
func TestCreateSession_InheritanceSurvivesAChain(t *testing.T) {
	db := openWorkstreamStore(t)

	ws, err := db.CreateWorkstream(WorkstreamRow{Name: "long haul"})
	if err != nil {
		t.Fatalf("CreateWorkstream: %v", err)
	}
	mustCreateSession(t, db, SessionRow{
		ID: "s1", State: "running", Intent: "fresh",
		WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
	})
	mustCreateSession(t, db, SessionRow{
		ID: "s2", State: "running", Intent: "compact", ParentSessionID: parentRef("s1"),
	})
	s3 := mustCreateSession(t, db, SessionRow{
		ID: "s3", State: "running", Intent: "resume", ParentSessionID: parentRef("s2"),
	})
	if s3.WorkstreamID.String != ws.ID {
		t.Errorf("workstream after fresh->compact->resume = %q, want %q", s3.WorkstreamID.String, ws.ID)
	}
}

// An explicit assignment on the incoming row is not overwritten by inheritance.
func TestCreateSession_ExplicitWorkstreamBeatsInheritance(t *testing.T) {
	db := openWorkstreamStore(t)

	inherited, err := db.CreateWorkstream(WorkstreamRow{Name: "parent's"})
	if err != nil {
		t.Fatalf("CreateWorkstream: %v", err)
	}
	explicit, err := db.CreateWorkstream(WorkstreamRow{Name: "caller's"})
	if err != nil {
		t.Fatalf("CreateWorkstream: %v", err)
	}
	mustCreateSession(t, db, SessionRow{
		ID: "p", State: "running", Intent: "fresh",
		WorkstreamID: sql.NullString{String: inherited.ID, Valid: true},
	})
	child := mustCreateSession(t, db, SessionRow{
		ID: "c", State: "running", Intent: "fork", ParentSessionID: parentRef("p"),
		WorkstreamID: sql.NullString{String: explicit.ID, Valid: true},
	})
	if child.WorkstreamID.String != explicit.ID {
		t.Errorf("workstream = %q, want the explicit %q", child.WorkstreamID.String, explicit.ID)
	}
}

// 'fresh' has no lineage and 'preassigned' describes how the id was chosen
// rather than that the work continues something, so neither inherits. Asserted
// so that widening lineageIntents is a deliberate act with a failing test
// attached, not a silent behavior change.
func TestCreateSession_NonLineageIntentsDoNotInherit(t *testing.T) {
	for _, intent := range []string{"fresh", "preassigned"} {
		t.Run(intent, func(t *testing.T) {
			db := openWorkstreamStore(t)
			ws, err := db.CreateWorkstream(WorkstreamRow{})
			if err != nil {
				t.Fatalf("CreateWorkstream: %v", err)
			}
			mustCreateSession(t, db, SessionRow{
				ID: "p", State: "running", Intent: "fresh",
				WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
			})
			child := mustCreateSession(t, db, SessionRow{
				ID: "c", State: "running", Intent: intent, ParentSessionID: parentRef("p"),
			})
			if child.WorkstreamID.Valid {
				t.Errorf("%s inherited %q; only resume/compact/fork should", intent, child.WorkstreamID.String)
			}
		})
	}
}

// A missing parent must not fail the launch. Inheritance is best-effort.
func TestCreateSession_MissingParentDoesNotFailCreation(t *testing.T) {
	db := openWorkstreamStore(t)
	child := mustCreateSession(t, db, SessionRow{
		ID: "orphan", State: "running", Intent: "resume", ParentSessionID: parentRef("nobody"),
	})
	if child.WorkstreamID.Valid {
		t.Errorf("workstream = %q, want none", child.WorkstreamID.String)
	}
}

// "A fresh session with no workstream can obtain one in one call."
func TestEnsureSessionWorkstream_CreatesOnDemand(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "solo", State: "running", Intent: "fresh"})

	ws, err := db.EnsureSessionWorkstream("solo", WorkstreamRow{Name: "on demand"})
	if err != nil {
		t.Fatalf("EnsureSessionWorkstream: %v", err)
	}
	if ws.ID == "" {
		t.Fatal("no workstream id returned")
	}
	got, err := db.GetSession("solo")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.WorkstreamID.String != ws.ID {
		t.Errorf("session workstream = %q, want %q", got.WorkstreamID.String, ws.ID)
	}

	// Idempotent: a second call returns the same container, not a new one.
	again, err := db.EnsureSessionWorkstream("solo", WorkstreamRow{Name: "should not be used"})
	if err != nil {
		t.Fatalf("EnsureSessionWorkstream (2nd): %v", err)
	}
	if again.ID != ws.ID {
		t.Errorf("second call minted %q, want the existing %q", again.ID, ws.ID)
	}
}

// Creating on demand from a descendant must stamp the whole lineage, not just
// the caller. Stamping only the caller would leave its own parent outside the
// container — the same orphaning S1 exists to prevent, pointed the other way.
func TestEnsureSessionWorkstream_StampsTheWholeLineage(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "root", State: "running", Intent: "fresh"})
	mustCreateSession(t, db, SessionRow{
		ID: "mid", State: "running", Intent: "compact", ParentSessionID: parentRef("root"),
	})
	mustCreateSession(t, db, SessionRow{
		ID: "leaf", State: "running", Intent: "resume", ParentSessionID: parentRef("mid"),
	})

	ws, err := db.EnsureSessionWorkstream("leaf", WorkstreamRow{Name: "late container"})
	if err != nil {
		t.Fatalf("EnsureSessionWorkstream: %v", err)
	}
	for _, id := range []string{"root", "mid", "leaf"} {
		got, err := db.GetSession(id)
		if err != nil {
			t.Fatalf("GetSession(%q): %v", id, err)
		}
		if got.WorkstreamID.String != ws.ID {
			t.Errorf("session %q workstream = %q, want %q", id, got.WorkstreamID.String, ws.ID)
		}
	}
}

// A sibling created BEFORE the workstream existed inherited nothing at
// creation. It must converge on the same container rather than mint a second
// one for the same work.
func TestEnsureSessionWorkstream_SiblingConvergesOnTheSameContainer(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "root", State: "running", Intent: "fresh"})
	mustCreateSession(t, db, SessionRow{
		ID: "fork-a", State: "running", Intent: "fork", ParentSessionID: parentRef("root"),
	})
	mustCreateSession(t, db, SessionRow{
		ID: "fork-b", State: "running", Intent: "fork", ParentSessionID: parentRef("root"),
	})

	first, err := db.EnsureSessionWorkstream("fork-a", WorkstreamRow{Name: "shared"})
	if err != nil {
		t.Fatalf("EnsureSessionWorkstream(fork-a): %v", err)
	}
	second, err := db.EnsureSessionWorkstream("fork-b", WorkstreamRow{Name: "should not be used"})
	if err != nil {
		t.Fatalf("EnsureSessionWorkstream(fork-b): %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("sibling got workstream %q, want the shared %q", second.ID, first.ID)
	}
}

// parent_session_id carries no enforced FK (ADR-0008) and nothing rejects a
// cycle on write, so the walk must fail rather than spin.
func TestSessionLineage_CycleIsAnErrorNotAHang(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "a", State: "running", Intent: "fresh"})
	mustCreateSession(t, db, SessionRow{
		ID: "b", State: "running", Intent: "resume", ParentSessionID: parentRef("a"),
	})
	if _, err := db.db.Exec(`UPDATE sessions SET parent_session_id='b' WHERE id='a'`); err != nil {
		t.Fatalf("forge cycle: %v", err)
	}
	if _, err := db.sessionLineage("a"); err == nil {
		t.Error("cycle accepted; want an error")
	}
}

func TestWorkstream_CreateGetListAssign(t *testing.T) {
	db := openWorkstreamStore(t)

	ws, err := db.CreateWorkstream(WorkstreamRow{Name: "named", WorkflowID: "wf-9"})
	if err != nil {
		t.Fatalf("CreateWorkstream: %v", err)
	}
	if ws.ID == "" || ws.Status != "active" {
		t.Fatalf("unexpected created row: %+v", ws)
	}

	got, err := db.GetWorkstream(ws.ID)
	if err != nil {
		t.Fatalf("GetWorkstream: %v", err)
	}
	if got.Name != "named" || got.WorkflowID != "wf-9" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	if _, err := db.GetWorkstream("nope"); !errors.Is(err, ErrWorkstreamNotFound) {
		t.Errorf("GetWorkstream(missing) = %v, want ErrWorkstreamNotFound", err)
	}

	list, err := db.ListWorkstreams(ListWorkstreamsOptions{WorkflowID: "wf-9"})
	if err != nil {
		t.Fatalf("ListWorkstreams: %v", err)
	}
	if len(list) != 1 || list[0].ID != ws.ID {
		t.Errorf("list = %+v, want just %q", list, ws.ID)
	}

	mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})
	if err := db.AssignSessionWorkstream("s", ws.ID); err != nil {
		t.Fatalf("AssignSessionWorkstream: %v", err)
	}
	if err := db.AssignSessionWorkstream("s", "ghost"); !errors.Is(err, ErrWorkstreamNotFound) {
		t.Errorf("assign to missing workstream = %v, want ErrWorkstreamNotFound", err)
	}
	if err := db.AssignSessionWorkstream("ghost-session", ws.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("assign to missing session = %v, want ErrSessionNotFound", err)
	}
}
