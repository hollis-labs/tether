package store

import (
	"database/sql"
	"testing"
)

// "Attaching the same ref twice succeeds and yields one row." The hooks that
// write git refs can run twice; the second run must be a no-op, not an error.
func TestAttachSessionRef_IsIdempotent(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})

	ref := SessionRefRow{
		SessionID: "s", Kind: "git_commit", RefID: "37b4dc0",
		Relation: RelationCreated, Source: SourceAPI,
	}
	res, err := db.AttachSessionRef(ref)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if !res.Inserted {
		t.Error("first attach reported Inserted=false")
	}

	res, err = db.AttachSessionRef(ref)
	if err != nil {
		t.Fatalf("second attach must not error: %v", err)
	}
	if res.Inserted || res.Upgraded {
		t.Errorf("second attach did something: %+v; want a pure no-op", res)
	}

	got, err := db.ListSessionRefs("s", ListSessionRefsOptions{})
	if err != nil {
		t.Fatalf("ListSessionRefs: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d rows, want exactly 1", len(got))
	}
}

// The UNIQUE key includes relation, so the same object read and then created
// is two distinct facts, not a collision.
func TestAttachSessionRef_RelationIsPartOfIdentity(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})

	for _, rel := range []string{RelationRead, RelationUpdated} {
		if _, err := db.AttachSessionRef(SessionRefRow{
			SessionID: "s", Kind: "torque_task", RefID: "CW-1", Relation: rel, Source: SourceProxy,
		}); err != nil {
			t.Fatalf("attach %s: %v", rel, err)
		}
	}
	got, err := db.ListSessionRefs("s", ListSessionRefsOptions{})
	if err != nil {
		t.Fatalf("ListSessionRefs: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d rows, want 2 (read and updated are different facts)", len(got))
	}
}

// A repeat must not rewrite claims about WHAT HAPPENED. relation, ref_id, uri
// and at are those claims; letting a later write revise them would turn an
// audit trail into a current view.
func TestAttachSessionRef_RepeatDoesNotRewriteTheFacts(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})

	base := SessionRefRow{
		SessionID: "s", Kind: KindTorqueTask, RefID: "CW-1",
		Relation: RelationRead, Source: SourceAgent, At: "2026-01-01T00:00:00Z",
		URI: "first",
	}
	if _, err := db.AttachSessionRef(base); err != nil {
		t.Fatalf("attach: %v", err)
	}
	revised := base
	revised.URI = "second"
	revised.At = "2026-06-01T00:00:00Z"
	if _, err := db.AttachSessionRef(revised); err != nil {
		t.Fatalf("re-attach: %v", err)
	}

	got, err := db.ListSessionRefs("s", ListSessionRefsOptions{})
	if err != nil {
		t.Fatalf("ListSessionRefs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].URI != "first" || got[0].At != "2026-01-01T00:00:00Z" {
		t.Errorf("a fact was rewritten: %+v", got[0])
	}
}

// source is the exception, and only upward. It is not a claim about the world
// but about how we know — so raising an assertion to a proxy OBSERVATION
// records better evidence for an unchanged fact. Leaving it alone would make a
// digest understate its own evidence, which is what the column exists to stop.
func TestAttachSessionRef_ProxyUpgradesAnAssertion(t *testing.T) {
	for _, weaker := range []string{SourceAgent, SourceAPI} {
		t.Run(weaker, func(t *testing.T) {
			db := openWorkstreamStore(t)
			mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})

			first := SessionRefRow{
				SessionID: "s", Kind: KindTorqueTask, RefID: "CW-1",
				Relation: RelationRead, Source: weaker, At: "2026-01-01T00:00:00Z",
			}
			if _, err := db.AttachSessionRef(first); err != nil {
				t.Fatalf("attach %s: %v", weaker, err)
			}
			observed := first
			observed.Source = SourceProxy
			observed.At = "2026-06-01T00:00:00Z"
			res, err := db.AttachSessionRef(observed)
			if err != nil {
				t.Fatalf("proxy attach: %v", err)
			}
			if !res.Upgraded || res.Inserted {
				t.Errorf("result = %+v, want Upgraded only", res)
			}

			got, err := db.ListSessionRefs("s", ListSessionRefsOptions{})
			if err != nil {
				t.Fatalf("ListSessionRefs: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d rows, want 1 — the upgrade must not add a row", len(got))
			}
			if got[0].Source != SourceProxy {
				t.Errorf("source = %q, want upgraded to proxy", got[0].Source)
			}
			// at records when it happened, not when we learned it better.
			if got[0].At != "2026-01-01T00:00:00Z" {
				t.Errorf("at = %q; the upgrade must not move it", got[0].At)
			}
		})
	}
}

// proxy is terminal: nothing downgrades an observation back to an assertion.
func TestAttachSessionRef_ProxyIsNeverDowngraded(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})

	base := SessionRefRow{
		SessionID: "s", Kind: KindTorqueTask, RefID: "CW-1",
		Relation: RelationRead, Source: SourceProxy,
	}
	if _, err := db.AttachSessionRef(base); err != nil {
		t.Fatalf("attach: %v", err)
	}
	for _, weaker := range []string{SourceAgent, SourceAPI} {
		down := base
		down.Source = weaker
		res, err := db.AttachSessionRef(down)
		if err != nil {
			t.Fatalf("attach %s: %v", weaker, err)
		}
		if res.Inserted || res.Upgraded {
			t.Errorf("%s did something to a proxy row: %+v", weaker, res)
		}
	}
	got, _ := db.ListSessionRefs("s", ListSessionRefsOptions{})
	if len(got) != 1 || got[0].Source != SourceProxy {
		t.Errorf("proxy row was disturbed: %+v", got)
	}
}

// api and agent are both assertions; neither is stronger, so neither overwrites
// the other.
func TestAttachSessionRef_AssertionsDoNotOverwriteEachOther(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})

	base := SessionRefRow{
		SessionID: "s", Kind: KindTorqueTask, RefID: "CW-1",
		Relation: RelationRead, Source: SourceAgent,
	}
	if _, err := db.AttachSessionRef(base); err != nil {
		t.Fatalf("attach: %v", err)
	}
	other := base
	other.Source = SourceAPI
	res, err := db.AttachSessionRef(other)
	if err != nil {
		t.Fatalf("attach api: %v", err)
	}
	if res.Inserted || res.Upgraded {
		t.Errorf("api overwrote agent: %+v", res)
	}
	got, _ := db.ListSessionRefs("s", ListSessionRefsOptions{})
	if len(got) != 1 || got[0].Source != SourceAgent {
		t.Errorf("source = %+v, want the original agent assertion", got)
	}
}

// The diagnostic the upgrade must not destroy: an assertion the proxy never
// observed stays a lone source=agent row, which is exactly how "the agent
// claimed X and nothing saw it" stays visible.
func TestAttachSessionRef_UnobservedAssertionStaysVisible(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})

	if _, err := db.AttachSessionRef(SessionRefRow{
		SessionID: "s", Kind: KindTorqueTask, RefID: "CW-claimed",
		Relation: RelationCreated, Source: SourceAgent,
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	got, err := db.ListSessionRefs("s", ListSessionRefsOptions{Source: SourceAgent})
	if err != nil {
		t.Fatalf("ListSessionRefs: %v", err)
	}
	if len(got) != 1 || got[0].RefID != "CW-claimed" {
		t.Errorf("unobserved assertion not findable: %+v", got)
	}
}

// THE acceptance criterion for S2: refs roll up to the workstream across a
// compact boundary. A compaction creates a NEW session row, so a roll-up that
// keyed on session_id would lose everything attached before it. This is S1's
// inheritance test extended to refs, which is what makes the two tasks one
// feature rather than two.
func TestListWorkstreamRefs_RollsUpAcrossACompactBoundary(t *testing.T) {
	db := openWorkstreamStore(t)

	ws, err := db.CreateWorkstream(WorkstreamRow{Name: "spans compaction"})
	if err != nil {
		t.Fatalf("CreateWorkstream: %v", err)
	}
	mustCreateSession(t, db, SessionRow{
		ID: "before", State: "running", Intent: "fresh",
		WorkstreamID: sql.NullString{String: ws.ID, Valid: true},
	})
	// The compaction. No workstream named by the caller — S1 inherits it.
	after := mustCreateSession(t, db, SessionRow{
		ID: "after", State: "running", Intent: "compact", ParentSessionID: parentRef("before"),
	})
	if after.WorkstreamID.String != ws.ID {
		t.Fatalf("precondition failed: compact child is in %q, not %q", after.WorkstreamID.String, ws.ID)
	}

	if _, err := db.AttachSessionRef(SessionRefRow{
		SessionID: "before", Kind: "torque_task", RefID: "CW-A",
		Relation: RelationCreated, Source: SourceProxy,
	}); err != nil {
		t.Fatalf("attach before: %v", err)
	}
	if _, err := db.AttachSessionRef(SessionRefRow{
		SessionID: "after", Kind: "git_commit", RefID: "abc1234",
		Relation: RelationCreated, Source: SourceAPI,
	}); err != nil {
		t.Fatalf("attach after: %v", err)
	}

	refs, err := db.ListWorkstreamRefs(ws.ID, ListSessionRefsOptions{})
	if err != nil {
		t.Fatalf("ListWorkstreamRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want both sides of the compaction", len(refs))
	}
	seen := map[string]bool{}
	for _, r := range refs {
		seen[r.RefID] = true
	}
	if !seen["CW-A"] || !seen["abc1234"] {
		t.Errorf("roll-up lost a side of the compaction: %+v", refs)
	}
}

func TestAttachSessionRef_RejectsInvalidVocabulary(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})

	if _, err := db.AttachSessionRef(SessionRefRow{
		SessionID: "s", Kind: "torque_task", RefID: "CW-1", Relation: "fiddled", Source: SourceAgent,
	}); err == nil {
		t.Error("invalid relation accepted")
	}
	if _, err := db.AttachSessionRef(SessionRefRow{
		SessionID: "s", Kind: "torque_task", RefID: "CW-1", Relation: RelationRead, Source: "vibes",
	}); err == nil {
		t.Error("invalid source accepted")
	}
	if _, err := db.AttachSessionRef(SessionRefRow{
		SessionID: "s", Kind: "", RefID: "CW-1",
	}); err == nil {
		t.Error("empty kind accepted")
	}
	if _, err := db.AttachSessionRef(SessionRefRow{
		SessionID: "s", Kind: "torque_task", RefID: "",
	}); err == nil {
		t.Error("empty ref_id accepted")
	}
}

func TestListSessionRefs_Filters(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", State: "running", Intent: "fresh"})

	for _, r := range []SessionRefRow{
		{SessionID: "s", Kind: "torque_task", RefID: "CW-1", Relation: RelationRead, Source: SourceProxy},
		{SessionID: "s", Kind: "git_commit", RefID: "aaa", Relation: RelationCreated, Source: SourceAPI},
	} {
		if _, err := db.AttachSessionRef(r); err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	got, err := db.ListSessionRefs("s", ListSessionRefsOptions{Kind: "git_commit"})
	if err != nil {
		t.Fatalf("ListSessionRefs: %v", err)
	}
	if len(got) != 1 || got[0].RefID != "aaa" {
		t.Errorf("kind filter = %+v, want just the commit", got)
	}
	got, err = db.ListSessionRefs("s", ListSessionRefsOptions{Source: SourceProxy})
	if err != nil {
		t.Fatalf("ListSessionRefs: %v", err)
	}
	if len(got) != 1 || got[0].RefID != "CW-1" {
		t.Errorf("source filter = %+v, want just the proxy-observed one", got)
	}
}

// The store has no column that accepts free-form content — the property that
// keeps it from becoming a second source of truth. Asserted against the schema
// so that adding one is a failing test, not a review someone has to catch.
func TestSessionRefs_SchemaHasNoContentColumn(t *testing.T) {
	db := openWorkstreamStore(t)
	rows, err := db.db.Query(`PRAGMA table_info(session_refs)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()

	want := map[string]bool{
		"id": true, "session_id": true, "kind": true, "ref_id": true,
		"uri": true, "relation": true, "source": true, "at": true,
	}
	got := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = true
		if !want[name] {
			t.Errorf("unexpected column %q: the kind/ref_id/uri triple is closed, and a content or blob column is what would turn this into a second source of truth", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing column %q", name)
		}
	}
}
