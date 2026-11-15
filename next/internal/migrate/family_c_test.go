package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	gostore "github.com/hollis-labs/libs/core/store"
)

// TestFamilyCCopies: the S12c family copies and verifies. go-messaging's tables keep their ids (a copied
// delivery checkpoint must not skip transitions), their sqlite_sequence and the NUL in an idempotency
// scope, byte for byte; on Postgres they are skipped and reported (no library store there).
func TestFamilyCCopies(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		e, tgt := newEngine(t, d)
		rep := mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
		if got := queryInt(t, tgt, `SELECT position FROM message_event_checkpoints WHERE source = 'delivery'`); got != 4 {
			t.Errorf("delivery checkpoint = %d, want the source's 4 (copied, not re-fenced)", got)
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM sessions`); got != 3 {
			t.Errorf("sessions: %d", got)
		}
		if d == gostore.DriverPostgres {
			if len(rep.Skipped) != 7 {
				t.Fatalf("skipped = %v, want the 7 go-messaging tables", rep.Skipped)
			}
			return
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
	if rep.OK() || !strings.Contains(strings.Join(rep.Problems, " "), "sessions") {
		t.Fatalf("problems = %v, want sessions reported", rep.Problems)
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
}
