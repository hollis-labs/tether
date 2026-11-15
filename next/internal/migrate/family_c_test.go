package migrate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/msgstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

func storetestRaw(t *testing.T) *gostore.Store { return storetest.Raw(t, gostore.DriverSQLite) }

// TestFamilyCCopies: the S12c family copies and verifies. go-messaging's tables keep their ids (a copied
// delivery checkpoint must not skip transitions), their sqlite_sequence and the NUL in an idempotency
// scope, byte for byte; on Postgres they are skipped and reported (no library store there).
func TestFamilyCCopies(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		e, tgt := newEngine(t, d)
		rep := mustOK(t)(e.Run(ctx, Options{Phase: phaseFor(d, PhaseFinal), Batch: 10}))
		if got := queryInt(t, tgt, `SELECT count(*) FROM sessions`); got != 3 {
			t.Errorf("sessions: %d", got)
		}
		if d == gostore.DriverPostgres {
			// No library store on Postgres: its 7 tables are skipped, and so are the delivery checkpoint
			// and the deferred transitions, which point into them (the target keeps its -1 seed).
			if len(rep.Skipped) != 9 {
				t.Fatalf("skipped = %v, want the 7 go-messaging tables, the checkpoints and the deferrals", rep.Skipped)
			}
			if got := queryInt(t, tgt, `SELECT position FROM message_event_checkpoints WHERE source = 'delivery'`); got != -1 {
				t.Errorf("delivery checkpoint on Postgres = %d, want the target's -1 seed (not copied without its transitions)", got)
			}
			return
		}
		if got := queryInt(t, tgt, `SELECT position FROM message_event_checkpoints WHERE source = 'delivery'`); got != 4 {
			t.Errorf("delivery checkpoint = %d, want the source's 4 (copied, not re-fenced)", got)
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM messaging_transitions WHERE id IN (1, 2, 4, 5)`); got != 4 {
			t.Errorf("transition ids not carried")
		}
		var scope []byte
		if err := tgt.Reader().QueryRowContext(ctx, `SELECT CAST(scope AS BLOB) FROM messaging_idempotency`).Scan(&scope); err != nil {
			t.Fatal(err)
		}
		if string(scope) != "msg://agent/a\x00key-1" {
			t.Errorf("scope = %q, want the NUL separator kept", scope)
		}
		var cls string
		if err := tgt.Reader().QueryRowContext(ctx, `SELECT typeof(payload) FROM messaging_messages`).Scan(&cls); err != nil || cls != "blob" {
			t.Errorf("library payload class %q %v", cls, err)
		}
	})
}

// TestFamilyCDriftReported: a changed session row is reported by its range, and a final run repairs it.
func TestFamilyCDriftReported(t *testing.T) {
	ctx := context.Background()
	e, tgt := newEngine(t, gostore.DriverSQLite)
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
	if _, err := tgt.ExecContext(ctx, `UPDATE sessions SET state = 'failed' WHERE id = 's-run'`); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Verify(ctx, Options{Batch: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range rep.Verify {
		for _, m := range v.Mismatched {
			// sessions is keyed by its TEXT id: the reported range must hold "s-run".
			found = found || (v.Table == "sessions" && m.Lo < "s-run" && (m.Hi == "" || m.Hi >= "s-run"))
		}
	}
	if rep.OK() || !found {
		t.Fatalf("problems = %v, want the sessions range holding s-run reported", rep.Problems)
	}
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
	var state string
	if err := tgt.Reader().QueryRowContext(ctx, `SELECT state FROM sessions WHERE id = 's-run'`).Scan(&state); err != nil || state != "running" {
		t.Fatalf("s-run state %q %v", state, err)
	}
}

// TestPrecheck: the stuck launch is found and named; the event identity is consistent in the fixture, and
// an event above the cutover without an event id is found.
func TestPrecheck(t *testing.T) {
	ctx := context.Background()
	path := oldFixture(t)
	check := func() map[string]Finding {
		t.Helper()
		src, err := OpenSource(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = src.Close() }()
		fs, err := New(src, path, nil).Precheck(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Finding{}
		for _, f := range fs {
			out[f.Check] = f
		}
		return out
	}
	got := check()
	if f := got["stuck_launches"]; f.OK || !strings.Contains(f.Detail, "s-stuck") || strings.Contains(f.Detail, "s-run") {
		t.Errorf("stuck_launches = %+v, want s-stuck named (and not the running session)", f)
	}
	db0, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sessions (id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, created_at, updated_at) VALUES ('s-reserved', 'l', 'p', 'agent-00', 'claude', '/w', 'created', 't', 't')`,
		`INSERT INTO workspace_scratch_allocations (session_id, operation_id, session_digest, plan_digest, root, status) VALUES ('s-reserved', 'op3', 'sd', 'pd', '/r3', 'reserved')`,
	} {
		if _, err := db0.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db0.Close()
	if f := check()["stuck_launches"]; !strings.Contains(f.Detail, "s-reserved") {
		t.Errorf("a created session with a reserved allocation is not named: %+v", f)
	}
	if f := got["event_identity"]; !f.OK {
		t.Errorf("event_identity = %+v, want ok on the fixture", f)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE events SET event_id = NULL WHERE id = 44`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if f := check()["event_identity"]; f.OK || !strings.Contains(f.Detail, "above the cutover") {
		t.Errorf("event_identity after a NULL event_id above the cutover = %+v", f)
	}
	for _, c := range []struct{ q, want string }{
		{`UPDATE event_identity SET next_legacy_id = cutover + 1`, "backfill cursor"},
		{`UPDATE sqlite_sequence SET seq = 1 WHERE name = 'events'`, "below the largest id"},
	} {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(c.q); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		if f := check()["event_identity"]; f.OK || !strings.Contains(f.Detail, c.want) {
			t.Errorf("after %q: event_identity = %+v, want %q", c.q, f, c.want)
		}
	}
	// A check whose table is missing fails on its own; the other still reports.
	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Exec(`DROP TABLE workspace_scratch_allocations`); err != nil {
		t.Fatal(err)
	}
	_ = db2.Close()
	got = check()
	if f := got["stuck_launches"]; f.OK || !strings.Contains(f.Detail, "could not run") {
		t.Errorf("stuck_launches without its table = %+v", f)
	}
	if _, ok := got["event_identity"]; !ok {
		t.Error("event_identity did not report when stuck_launches failed")
	}
}

// TestFinalRefusesUncoveredTables: a source table in no family and on no exclusion list refuses a final
// run before anything is written, and is named.
func TestFinalRefusesUncoveredTables(t *testing.T) {
	path := oldFixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE registry_entries_not_ported (urn TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	src, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	tgt := storetestRaw(t)
	rep, err := New(src, path, tgt).Run(context.Background(), Options{Phase: PhaseFinal})
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "registry_entries_not_ported") || rep == nil || len(rep.Uncovered) != 1 {
		t.Fatalf("err = %v, report %+v", err, rep)
	}
	if got := queryInt(t, tgt, `SELECT count(*) FROM messages`); got != 0 {
		t.Fatal("rows were written before the refusal")
	}
}

// TestVerifyCreatesNothing: --verify on a target without go-messaging's tables reports them, and creates
// nothing.
func TestVerifyCreatesNothing(t *testing.T) {
	ctx := context.Background()
	e, tgt := newEngine(t, gostore.DriverSQLite)
	rep, err := e.Verify(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := queryInt(t, tgt, `SELECT count(*) FROM sqlite_master WHERE name LIKE 'messaging_%'`); got != 0 {
		t.Fatalf("verify created %d library objects", got)
	}
	if len(rep.Skipped) != 7 || rep.OK() {
		t.Fatalf("skipped %v, ok %v: want the 7 library tables reported and the difference found", rep.Skipped, rep.OK())
	}
}

// TestActivateDeliveryAfterCopy: after the copy, the ported store's delivery start-up keeps the copied
// checkpoint (it moves only a -1 seed), and the next transition the library writes gets an id above the
// copied ones, so the event drain neither repeats nor skips one.
func TestActivateDeliveryAfterCopy(t *testing.T) {
	ctx := context.Background()
	e, tgt := newEngine(t, gostore.DriverSQLite)
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
	ms := msgstore.New(store.New(tgt))
	if err := ms.ActivateDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queryInt(t, tgt, `SELECT position FROM message_event_checkpoints WHERE source = 'delivery'`); got != 4 {
		t.Fatalf("delivery checkpoint after activation = %d, want the copied 4", got)
	}
	db, err := store.New(tgt).LibraryDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messaging_transitions (delivery_id, message_id, attempt_id, from_status, to_status, reason, at) VALUES ('ld-1', 'lm-1', 'at-6', 'a', 'b', '', 't')`); err != nil {
		t.Fatal(err)
	}
	if got := queryInt(t, tgt, `SELECT max(id) FROM messaging_transitions`); got != 6 {
		t.Fatalf("next transition id = %d, want 6 (after the copied 5)", got)
	}
	if got := queryInt(t, tgt, `SELECT count(*) FROM messaging_transitions WHERE id > 4`); got != 2 {
		t.Fatalf("transitions after the checkpoint: %d, want 2 (5 copied, 6 new)", got)
	}
}
