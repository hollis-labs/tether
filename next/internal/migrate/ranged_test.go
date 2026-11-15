package migrate

import (
	"context"
	"database/sql"
	"testing"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// TestRangedTablesUseFixedRanges: a table keyed by a single integer (events) is walked in fixed-width key
// ranges, and the run stores each non-empty range's digest for the next run.
func TestRangedTablesUseFixedRanges(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		e, tgt := newEngine(t, d)
		sp, err := specFor(ctx, e.src, tgt, "events")
		if err != nil || !sp.ranged() {
			t.Fatalf("events is not ranged: %v", err)
		}
		if msgs, _ := specFor(ctx, e.src, tgt, "messages"); msgs.ranged() {
			t.Fatal("messages (a TEXT key) must use the sequential walker")
		}
		mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10, Workers: 3}))
		// events ids 2..44 in ranges of 10: (0,10] (10,20] (20,30] (30,40] (40,50] all non-empty.
		if got := queryInt(t, tgt, `SELECT count(*) FROM tether_migration_ranges WHERE table_name = 'events'`); got != 5 {
			t.Fatalf("stored events range digests: %d, want 5", got)
		}
	})
}

// TestRetentionDeleteRewritesOneRange: with fixed ranges, deleting the oldest events (retention) between
// bulk and final changes only the range that held them; every other range is equal by its stored digest.
func TestRetentionDeleteRewritesOneRange(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		a := oldFixture(t)
		srcA, err := OpenSource(a)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = srcA.Close() }()
		tgt := storetest.Raw(t, d)
		mustOK(t)(New(srcA, a, tgt).Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
		b := copyFile(t, a)
		db, err := sql.Open("sqlite", b)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DELETE FROM events WHERE id IN (2, 3, 4)`); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		srcB, err := OpenSource(b)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = srcB.Close() }()
		final := mustOK(t)(New(srcB, b, tgt).Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
		for _, c := range final.Copy {
			if c.Table == "events" && c.Written != 1 {
				t.Fatalf("events: %d ranges rewritten, want 1 (only (0,10])", c.Written)
			}
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM events WHERE id IN (2, 3, 4)`); got != 0 {
			t.Fatal("retention deletes were not carried")
		}
	})
}

// TestStoredDigestsTrustTheTarget documents the trade-off: between a verified run and the next one, only
// migrate-from writes the target, so a range whose source is unchanged is not re-read there. A change
// made to the target by anything else in between is not seen by the final run; the full verification
// (migrate-from --verify, which walks every range on both sides) finds it.
func TestStoredDigestsTrustTheTarget(t *testing.T) {
	ctx := context.Background()
	e, tgt := newEngine(t, gostore.DriverSQLite)
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
	if _, err := tgt.ExecContext(ctx, `UPDATE events SET kind = 'tampered' WHERE id = 12`); err != nil {
		t.Fatal(err)
	}
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 10})) // trusts the stored digest
	rep, err := e.Verify(ctx, Options{Batch: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range rep.Verify {
		if v.Table == "events" && len(v.Mismatched) == 1 && v.Mismatched[0].Lo == "10" {
			found = true
		}
	}
	if !found || rep.OK() {
		t.Fatalf("the full verification did not report events range (10, 20]: %+v", rep.Problems)
	}
}

// TestFinalMustCoverEveryFamily: a final run marks the database complete, which is never rewritten, so a
// final run of one family is refused (the other family would never arrive).
func TestFinalMustCoverEveryFamily(t *testing.T) {
	ctx := context.Background()
	e, tgt := newEngine(t, gostore.DriverSQLite)
	for _, fam := range []string{"a", "b"} {
		if _, err := e.Run(ctx, Options{Phase: PhaseFinal, Tables: Families[fam]}); err == nil {
			t.Fatalf("a final run of family %s was accepted", fam)
		}
	}
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Tables: Families["a"]})) // a bulk run of one family is fine
	if got := queryInt(t, tgt, `SELECT count(*) FROM tether_migration_mark WHERE state = 'complete'`); got != 0 {
		t.Fatal("the target was marked complete")
	}
}

// TestStoredDigestsNeedTheSameWidth: digests stored at one range width are never trusted at another; the
// final run reads both sides and repairs a target change.
func TestStoredDigestsNeedTheSameWidth(t *testing.T) {
	ctx := context.Background()
	e, tgt := newEngine(t, gostore.DriverSQLite)
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
	if _, err := tgt.ExecContext(ctx, `UPDATE events SET kind = 'tampered' WHERE id = 12`); err != nil {
		t.Fatal(err)
	}
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 5}))
	if got := queryInt(t, tgt, `SELECT count(*) FROM events WHERE kind = 'tampered'`); got != 0 {
		t.Fatal("a different width trusted the stored digests")
	}
}

// TestStoredDigestsNeedAVerifiedBulk: digests are trusted only if the run that stored them left the mark at
// bulk with their run id; after anything else (here a run that never finished) both sides are read.
func TestStoredDigestsNeedAVerifiedBulk(t *testing.T) {
	ctx := context.Background()
	e, tgt := newEngine(t, gostore.DriverSQLite)
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
	for _, q := range []string{
		`UPDATE events SET kind = 'tampered' WHERE id = 12`,
		`UPDATE tether_migration_mark SET state = 'running'`, // as a run that stopped part way leaves it
	} {
		if _, err := tgt.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
	if got := queryInt(t, tgt, `SELECT count(*) FROM events WHERE kind = 'tampered'`); got != 0 {
		t.Fatal("digests from an unfinished run were trusted")
	}
}

// TestBadTargetCheckRefusedFirst: an unknown --target-check is refused before anything is written.
func TestBadTargetCheckRefusedFirst(t *testing.T) {
	ctx := context.Background()
	e, tgt := newEngine(t, gostore.DriverSQLite)
	if _, err := e.Run(ctx, Options{Phase: PhaseBulk, TargetCheck: "maybe"}); err == nil {
		t.Fatal("accepted")
	}
	if got := queryInt(t, tgt, `SELECT count(*) FROM messages`); got != 0 {
		t.Fatal("rows were written before the refusal")
	}
}
