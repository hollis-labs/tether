package store

import (
	"fmt"
	"strconv"
	"testing"
)

// buildLineage constructs fresh -> compact -> resume inside one workstream and
// returns the three session ids in that order.
//
// IT IS CONSTRUCTED ON PURPOSE, and that is the point of this whole file.
// Measured 2026-09-12 against the live DB: 129 sessions, every one intent
// 'fresh', not one with a parent, and a single workstream containing zero
// sessions. So the roll-up across a lineage -- the only behavior S5 exists for
// -- has NO production instance to validate against. A live check would find
// one empty workstream, pass, and have exercised none of this.
func buildLineage(t *testing.T, db *Store) (ws WorkstreamRow, fresh, compact, resume string) {
	t.Helper()
	root := mustCreateSession(t, db, SessionRow{ID: "s-fresh", Intent: "fresh", State: "completed"})
	ws, err := db.EnsureSessionWorkstream(root.ID, WorkstreamRow{Name: "lineage"})
	if err != nil {
		t.Fatalf("EnsureSessionWorkstream: %v", err)
	}
	mustCreateSession(t, db, SessionRow{
		ID: "s-compact", Intent: "compact", State: "completed",
		ParentSessionID: parentRef("s-fresh"),
	})
	mustCreateSession(t, db, SessionRow{
		ID: "s-resume", Intent: "resume", State: "running",
		ParentSessionID: parentRef("s-compact"),
	})
	return ws, "s-fresh", "s-compact", "s-resume"
}

func mustAttach(t *testing.T, db *Store, ref SessionRefRow) {
	t.Helper()
	if _, err := db.AttachSessionRef(ref); err != nil {
		t.Fatalf("AttachSessionRef(%s/%s): %v", ref.Kind, ref.RefID, err)
	}
}

// TestWorkstreamDigest_RollsUpAcrossACompaction is the sprint's headline
// acceptance: "a workstream spanning a compaction shows refs from BOTH
// sessions in one digest."
//
// It also pins the part that is easy to get accidentally right: the digest
// must span the lineage because S1's inheritance put all three sessions in one
// workstream, not because the query happened to be broad.
func TestWorkstreamDigest_RollsUpAcrossACompaction(t *testing.T) {
	db := openWorkstreamStore(t)
	ws, fresh, compact, resume := buildLineage(t, db)

	mustAttach(t, db, SessionRefRow{SessionID: fresh, Kind: KindTorqueTask, RefID: "CW-1", Relation: RelationCreated, Source: SourceAgent, At: "2026-09-12T01:00:00Z"})
	mustAttach(t, db, SessionRefRow{SessionID: compact, Kind: KindTorqueTask, RefID: "CW-2", Relation: RelationUpdated, Source: SourceProxy, At: "2026-09-12T02:00:00Z"})
	mustAttach(t, db, SessionRefRow{SessionID: resume, Kind: KindTesseractRevision, RefID: "01J9ABC", Relation: RelationRead, Source: SourceAPI, At: "2026-09-12T03:00:00Z"})

	d, err := db.WorkstreamDigest(ws.ID, DigestOptions{})
	if err != nil {
		t.Fatalf("WorkstreamDigest: %v", err)
	}
	if d.Totals.Refs != 3 {
		t.Fatalf("refs = %d, want 3 (one per session in the lineage)", d.Totals.Refs)
	}
	if d.Span.SessionCount != 3 {
		t.Fatalf("session_count = %d, want 3", d.Span.SessionCount)
	}
	if !d.Span.SpansLineage {
		t.Fatal("spans_lineage = false; the compact and resume children both have parents inside the span")
	}

	// The narrower grain must NOT roll up, or there is no way to ask the
	// narrower question.
	sd, err := db.SessionDigest(fresh, DigestOptions{})
	if err != nil {
		t.Fatalf("SessionDigest: %v", err)
	}
	if sd.Totals.Refs != 1 {
		t.Fatalf("session digest refs = %d, want 1: the session grain is this session's own refs, not the roll-up", sd.Totals.Refs)
	}
	if sd.Workstream == nil || sd.Workstream.ID != ws.ID {
		t.Fatal("session digest must carry its workstream as the escalation path to the roll-up")
	}
}

// TestDigest_SpansLineageNeedsAParentInTheSpan pins the narrower definition.
//
// An earlier draft of this field meant SessionCount > 1. Three sessions
// assembled by manual assigns have not crossed a compaction, and reporting
// true for them would hide the single fact the field exists to expose --
// "large" and "crossed a compaction" are different claims and only the second
// is evidence the container did its job.
func TestDigest_SpansLineageNeedsAParentInTheSpan(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "a", Intent: "fresh", State: "completed"})
	mustCreateSession(t, db, SessionRow{ID: "b", Intent: "fresh", State: "completed"})
	ws, err := db.CreateWorkstream(WorkstreamRow{Name: "manual"})
	if err != nil {
		t.Fatalf("CreateWorkstream: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if err := db.AssignSessionWorkstream(id, ws.ID); err != nil {
			t.Fatalf("AssignSessionWorkstream(%q): %v", id, err)
		}
	}
	d, err := db.WorkstreamDigest(ws.ID, DigestOptions{})
	if err != nil {
		t.Fatalf("WorkstreamDigest: %v", err)
	}
	if d.Span.SessionCount != 2 {
		t.Fatalf("session_count = %d, want 2", d.Span.SessionCount)
	}
	if d.Span.SpansLineage {
		t.Fatal("spans_lineage = true for two unrelated sessions; count is not lineage")
	}
}

// TestDigest_DistinguishesNothingHappenedFromNotAttributable is the
// requirement carried into S5 from the S3 approach review: an empty digest
// must not read the same way for a session that did nothing and a session that
// could never have produced an observed ref.
func TestDigest_DistinguishesNothingHappenedFromNotAttributable(t *testing.T) {
	db := openWorkstreamStore(t)
	ws, err := db.CreateWorkstream(WorkstreamRow{Name: "mixed"})
	if err != nil {
		t.Fatalf("CreateWorkstream: %v", err)
	}
	cases := []struct {
		id, attribution string
	}{
		{"s-proxy", RefAttributionProxy},
		{"s-none", RefAttributionNone},
		{"s-external", RefAttributionUnlaunched},
		{"s-legacy", ""}, // predates the stamp
	}
	for _, c := range cases {
		mustCreateSession(t, db, SessionRow{ID: c.id, Intent: "fresh", State: "completed"})
		if err := db.AssignSessionWorkstream(c.id, ws.ID); err != nil {
			t.Fatalf("AssignSessionWorkstream(%q): %v", c.id, err)
		}
		if c.attribution != "" {
			if err := db.SetSessionRefAttribution(c.id, c.attribution); err != nil {
				t.Fatalf("SetSessionRefAttribution(%q): %v", c.id, err)
			}
		}
	}

	d, err := db.WorkstreamDigest(ws.ID, DigestOptions{})
	if err != nil {
		t.Fatalf("WorkstreamDigest: %v", err)
	}
	if d.Totals.Refs != 0 {
		t.Fatalf("refs = %d, want 0: this case is about how emptiness is EXPLAINED", d.Totals.Refs)
	}
	if d.Coverage.ProxyAttributable != 1 {
		t.Fatalf("proxy_attributable = %d, want 1 (only s-proxy could produce one)", d.Coverage.ProxyAttributable)
	}
	want := map[string]int{
		RefAttributionProxy: 1, RefAttributionNone: 1,
		RefAttributionUnlaunched: 1, RefAttributionUnknown: 1,
	}
	for k, n := range want {
		if d.Coverage.Attribution[k] != n {
			t.Fatalf("attribution[%q] = %d, want %d (full map: %v)", k, d.Coverage.Attribution[k], n, d.Coverage.Attribution)
		}
	}
	// The NULL row must read as "unknown", never as "none": one says extraction
	// was off, the other says nothing recorded what was planted.
	for _, s := range d.Span.Sessions {
		if s.ID == "s-legacy" && s.RefAttribution != RefAttributionUnknown {
			t.Fatalf("s-legacy attribution = %q, want %q", s.RefAttribution, RefAttributionUnknown)
		}
	}
}

// TestDigest_SplitsLeftBehindFromTouched pins the split the task names: "Touched
// 14 tasks" is noise, "created 2, updated 3, read 9" is the answer.
func TestDigest_SplitsLeftBehindFromTouched(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", Intent: "fresh", State: "completed"})
	for _, c := range []struct{ refID, relation string }{
		{"CW-created", RelationCreated},
		{"CW-updated", RelationUpdated},
		{"CW-read", RelationRead},
		{"CW-referenced", RelationReferenced},
	} {
		mustAttach(t, db, SessionRefRow{
			SessionID: "s", Kind: KindTorqueTask, RefID: c.refID,
			Relation: c.relation, Source: SourceAgent, At: "2026-09-12T01:00:00Z",
		})
	}
	d, err := db.SessionDigest("s", DigestOptions{})
	if err != nil {
		t.Fatalf("SessionDigest: %v", err)
	}
	if len(d.LeftBehind) != 1 || len(d.LeftBehind[0].Created) != 1 || len(d.LeftBehind[0].Updated) != 1 {
		t.Fatalf("left_behind = %+v, want one kind carrying one created and one updated", d.LeftBehind)
	}
	if len(d.Touched) != 1 || len(d.Touched[0].Read) != 1 || len(d.Touched[0].Referenced) != 1 {
		t.Fatalf("touched = %+v, want one kind carrying one read and one referenced", d.Touched)
	}
	// Nothing may appear in both sections, or a reader counting "what did this
	// produce" double-counts what it merely consulted.
	if len(d.LeftBehind[0].Read) != 0 || len(d.Touched[0].Created) != 0 {
		t.Fatal("a relation leaked across the left_behind / touched split")
	}
}

// TestDigest_ReportsTruncationRatherThanHidingIt. A digest that silently drops
// refs lets a reader conclude work did not happen because the list ended.
func TestDigest_ReportsTruncationRatherThanHidingIt(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", Intent: "fresh", State: "completed"})
	for i := 0; i < 5; i++ {
		mustAttach(t, db, SessionRefRow{
			SessionID: "s", Kind: KindTorqueTask,
			RefID:    "CW-" + strconv.Itoa(i),
			Relation: RelationRead, Source: SourceAgent,
			At: fmt.Sprintf("2026-09-12T0%d:00:00Z", i+1),
		})
	}
	d, err := db.SessionDigest("s", DigestOptions{Limit: 3})
	if err != nil {
		t.Fatalf("SessionDigest: %v", err)
	}
	if d.Totals.Refs != 3 {
		t.Fatalf("refs = %d, want 3 (the limit)", d.Totals.Refs)
	}
	if !d.Coverage.Truncated {
		t.Fatal("truncated = false with 5 refs and a limit of 3")
	}
	if d.Coverage.Limit != 3 {
		t.Fatalf("coverage.limit = %d, want 3", d.Coverage.Limit)
	}

	// Exactly at the limit is NOT truncation. Reporting it as truncated would
	// be the same failure pointed the other way -- a complete answer marked
	// incomplete sends a reader looking for refs that do not exist.
	d, err = db.SessionDigest("s", DigestOptions{Limit: 5})
	if err != nil {
		t.Fatalf("SessionDigest: %v", err)
	}
	if d.Coverage.Truncated {
		t.Fatal("truncated = true when the limit exactly matched the ref count")
	}
}

// TestDigest_SinceBoundsToWhatWasInFlight.
func TestDigest_SinceBoundsToWhatWasInFlight(t *testing.T) {
	db := openWorkstreamStore(t)
	mustCreateSession(t, db, SessionRow{ID: "s", Intent: "fresh", State: "completed"})
	mustAttach(t, db, SessionRefRow{SessionID: "s", Kind: KindTorqueTask, RefID: "old", Relation: RelationRead, Source: SourceAgent, At: "2026-09-10T00:00:00Z"})
	mustAttach(t, db, SessionRefRow{SessionID: "s", Kind: KindTorqueTask, RefID: "new", Relation: RelationRead, Source: SourceAgent, At: "2026-09-12T00:00:00Z"})

	d, err := db.SessionDigest("s", DigestOptions{Since: "2026-09-11T00:00:00Z"})
	if err != nil {
		t.Fatalf("SessionDigest: %v", err)
	}
	if d.Totals.Refs != 1 {
		t.Fatalf("refs = %d, want 1", d.Totals.Refs)
	}
	if d.Touched[0].Read[0].RefID != "new" {
		t.Fatalf("kept %q, want the ref after the bound", d.Touched[0].Read[0].RefID)
	}
}

// TestParseRefSelector_SplitsOnTheFirstColonOnly.
//
// The failure this guards is silent. `msg://agent/agent-mux/agt_x9k2p4`
// contains colons, and splitting on every colon yields kind="messaging_urn",
// ref_id="msg" -- which matches nothing, returns an empty list and no error,
// and reads as "no workstream touched it".
func TestParseRefSelector_SplitsOnTheFirstColonOnly(t *testing.T) {
	kind, refID, err := ParseRefSelector("messaging_urn:msg://agent/agent-mux/agt_x9k2p4")
	if err != nil {
		t.Fatalf("ParseRefSelector: %v", err)
	}
	if kind != "messaging_urn" {
		t.Fatalf("kind = %q, want messaging_urn", kind)
	}
	if refID != "msg://agent/agent-mux/agt_x9k2p4" {
		t.Fatalf("ref_id = %q; the colons inside the value must survive", refID)
	}

	if _, _, err := ParseRefSelector("torque_task"); err == nil {
		t.Fatal("a selector with no colon must error rather than guess a kind")
	}
	if _, _, err := ParseRefSelector(":CW-1"); err == nil {
		t.Fatal("an empty kind must error")
	}
	if _, _, err := ParseRefSelector("torque_task:"); err == nil {
		t.Fatal("an empty ref_id must error")
	}
}

// TestWorkstreamsForRef_ReturnsEveryMatch. Two efforts touching one task is
// ordinary; picking one would look authoritative and be wrong.
func TestWorkstreamsForRef_ReturnsEveryMatch(t *testing.T) {
	db := openWorkstreamStore(t)
	for i, id := range []string{"s-a", "s-b"} {
		mustCreateSession(t, db, SessionRow{ID: id, Intent: "fresh", State: "completed"})
		ws, err := db.CreateWorkstream(WorkstreamRow{Name: string(rune('A' + i))})
		if err != nil {
			t.Fatalf("CreateWorkstream: %v", err)
		}
		if err := db.AssignSessionWorkstream(id, ws.ID); err != nil {
			t.Fatalf("AssignSessionWorkstream: %v", err)
		}
		mustAttach(t, db, SessionRefRow{
			SessionID: id, Kind: KindTorqueTask, RefID: "CW-20260911-0039",
			Relation: RelationUpdated, Source: SourceAgent, At: "2026-09-12T01:00:00Z",
		})
	}
	got, err := db.WorkstreamsForRef(KindTorqueTask, "CW-20260911-0039")
	if err != nil {
		t.Fatalf("WorkstreamsForRef: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("matched %d workstreams, want 2: both touched the task and neither may be dropped", len(got))
	}

	// A session with no workstream contributes no match rather than a blank
	// one -- there is no container to return.
	mustCreateSession(t, db, SessionRow{ID: "s-loose", Intent: "fresh", State: "completed"})
	mustAttach(t, db, SessionRefRow{
		SessionID: "s-loose", Kind: KindTorqueTask, RefID: "CW-20260911-0039",
		Relation: RelationRead, Source: SourceAgent, At: "2026-09-12T02:00:00Z",
	})
	got, err = db.WorkstreamsForRef(KindTorqueTask, "CW-20260911-0039")
	if err != nil {
		t.Fatalf("WorkstreamsForRef: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("matched %d, want 2: a container-less session must not produce an empty workstream entry", len(got))
	}
}
