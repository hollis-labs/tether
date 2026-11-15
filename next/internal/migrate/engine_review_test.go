package migrate

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// TestMigrateFromNeverRewritesACompleteTarget: once complete (it may have served), a run is refused and
// the rows written since stay; Verify re-checks without writing.
func TestMigrateFromNeverRewritesACompleteTarget(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		e, tgt := newEngine(t, d)
		mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
		if _, err := tgt.ExecContext(ctx, `INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES ('after-cutover', 'k', 'f', 't', 'c')`); err != nil {
			t.Fatal(err)
		}
		for _, p := range []Phase{PhaseBulk, PhaseFinal} {
			if _, err := e.Run(ctx, Options{Phase: p, Batch: 10}); !errors.Is(err, ErrTargetComplete) {
				t.Fatalf("%s run on a complete target: %v, want ErrTargetComplete", p, err)
			}
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM messages WHERE id = 'after-cutover'`); got != 1 {
			t.Fatal("a post-cutover row was lost")
		}
		rep, err := e.Verify(ctx, Options{Batch: 10})
		if err != nil {
			t.Fatal(err)
		}
		if rep.OK() || !strings.Contains(strings.Join(rep.Problems, " "), "messages") {
			t.Fatalf("verify should report the post-cutover row as a difference: %v", rep.Problems)
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM messages WHERE id = 'after-cutover'`); got != 1 {
			t.Fatal("Verify wrote to the target")
		}
	})
}

// TestDigestSeesStorageClass: on SQLite a value keeps its class through the copy, and a change of class
// (text to blob with the same bytes) is a difference the verification reports.
func TestDigestSeesStorageClass(t *testing.T) {
	ctx := context.Background()
	e, tgt := newEngine(t, gostore.DriverSQLite)
	mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
	var cls string
	if err := tgt.Reader().QueryRowContext(ctx, `SELECT typeof(payload) FROM team_definitions WHERE version = 2`).Scan(&cls); err != nil || cls != "text" {
		t.Fatalf("text stored in a BLOB column came back as %q, %v", cls, err)
	}
	if err := tgt.Reader().QueryRowContext(ctx, `SELECT typeof(payload) FROM messages WHERE id = 'm-blob'`).Scan(&cls); err != nil || cls != "blob" {
		t.Fatalf("a blob stored in a TEXT column came back as %q, %v", cls, err)
	}
	if _, err := tgt.ExecContext(ctx, `UPDATE messages SET payload = CAST(payload AS BLOB) WHERE id = 'm-001'`); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Verify(ctx, Options{Batch: 10})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("a text value turned blob (same bytes) passed verification")
	}
}

// TestPostgresConversionsAreCounted: the Postgres target stores a blob from a TEXT column as text and text
// from a BLOB column as bytes; each conversion is counted in the report, and the copy verifies.
func TestPostgresConversionsAreCounted(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(t, gostore.DriverPostgres)
	rep := mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
	if rep.Conversions["messages.payload"] != 1 || rep.Conversions["team_definitions.payload"] != 1 || rep.Conversions["principals.display"] != 1 {
		t.Fatalf("conversions = %v, want messages.payload, team_definitions.payload and principals.display 1 each", rep.Conversions)
	}
}

// TestEmptyBlobStaysEmpty: X” copies as X”, not NULL, and a final run over the copy writes nothing.
func TestEmptyBlobStaysEmpty(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		e, tgt := newEngine(t, d)
		mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
		if got := queryInt(t, tgt, `SELECT count(*) FROM team_definitions WHERE version = 3 AND payload IS NOT NULL`); got != 1 {
			t.Fatal("an empty blob was copied as NULL")
		}
		final := mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
		for _, c := range final.Copy {
			if c.Written != 0 {
				t.Errorf("final rewrote %d ranges of %s", c.Written, c.Table)
			}
		}
	})
}

// copyFile copies a source fixture, to make a second snapshot of it.
func copyFile(t *testing.T, from string) string {
	t.Helper()
	to := filepath.Join(t.TempDir(), "snapshot.db")
	in, err := os.Open(from) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return to
}

// TestRowidReuseConverges: between bulk and final the newest message is deleted and a new one takes its
// rowid with a key that sorts into an earlier range. The final run frees the rowid and converges.
func TestRowidReuseConverges(t *testing.T) {
	ctx := context.Background()
	a := oldFixture(t)
	srcA, err := OpenSource(a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srcA.Close() }()
	tgt := storetest.Raw(t, gostore.DriverSQLite)
	mustOK(t)(New(srcA, a, tgt).Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))

	b := copyFile(t, a)
	db, err := sql.Open("sqlite", b)
	if err != nil {
		t.Fatal(err)
	}
	var newest int64
	if err := db.QueryRow(`SELECT max(rowid) FROM messages`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM messages WHERE rowid = ?`, newest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES ('a-early', 'k', 'f', 't', 'c')`); err != nil {
		t.Fatal(err)
	}
	var reused int64
	if err := db.QueryRow(`SELECT rowid FROM messages WHERE id = 'a-early'`).Scan(&reused); err != nil || reused != newest {
		t.Fatalf("fixture: the new row took rowid %d, want the freed %d (%v)", reused, newest, err)
	}
	_ = db.Close()
	srcB, err := OpenSource(b)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srcB.Close() }()
	for i := 0; i < 2; i++ { // and a rerun is a no-op
		mustOK(t)(New(srcB, b, tgt).Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
	}
	if got := queryInt(t, tgt, `SELECT rowid FROM messages WHERE id = 'a-early'`); got != newest {
		t.Fatalf("a-early has rowid %d, want %d", got, newest)
	}
}

// TestNullKeyRefused: a source row with a NULL primary key is refused up front, with where it is.
func TestNullKeyRefused(t *testing.T) {
	path := oldFixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES (NULL, 'k', 'f', 't', 'c')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	src, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	_, err = New(src, path, storetest.Raw(t, gostore.DriverSQLite)).Run(context.Background(), Options{Phase: PhaseBulk})
	if !errors.Is(err, ErrNullKey) || !strings.Contains(err.Error(), "messages") {
		t.Fatalf("err = %v, want ErrNullKey naming messages", err)
	}
}

// TestCheckSource: a symlink is resolved before the checks; a -wal, -shm or -journal beside the file, or a
// file that is not SQLite, is refused. A WAL-mode header without sidecars (a .backup copy) is accepted and
// then requires the source check.
func TestCheckSource(t *testing.T) {
	good := oldFixture(t)
	if got, err := CheckSource(good); err != nil || got == "" {
		t.Fatalf("a rollback-mode copy: %q %v", got, err)
	}
	for _, side := range []string{"-wal", "-shm", "-journal"} {
		live := copyFile(t, good)
		if err := os.WriteFile(live+side, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link.db")
		if err := os.Symlink(live, link); err != nil {
			t.Fatal(err)
		}
		if _, err := CheckSource(link); !errors.Is(err, ErrLiveSource) {
			t.Errorf("a symlink to a file with %s beside it: %v, want ErrLiveSource", side, err)
		}
	}
	wal := copyFile(t, good)
	db, err := sql.Open("sqlite", wal)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q %v", mode, err)
	}
	_ = db.Close()
	for _, side := range []string{"-wal", "-shm"} {
		_ = os.Remove(wal + side) // a raw copy of the main file alone: the WAL is lost
	}
	// A WAL-mode header with no sidecar (a .backup copy of a WAL database) passes CheckSource, and then
	// needs the source check: with "none" the run refuses it.
	if _, err := CheckSource(wal); err != nil {
		t.Errorf("a WAL-mode copy with no sidecar: %v, want accepted", err)
	}
	src, err := OpenSource(wal)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	_, err = New(src, wal, storetest.Raw(t, gostore.DriverSQLite)).Run(context.Background(), Options{Phase: PhaseBulk, SourceCheck: "none"})
	if !errors.Is(err, ErrLiveSource) {
		t.Errorf("a WAL-mode source with no source check: %v, want ErrLiveSource", err)
	}
	rep := mustOK(t)(New(src, wal, storetest.Raw(t, gostore.DriverSQLite)).Run(context.Background(), Options{Phase: PhaseBulk, SourceCheck: "quick"}))
	if !rep.SourceWALHeader {
		t.Error("the report does not record the WAL-mode header")
	}
	notDB := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(notDB, []byte(strings.Repeat("x", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckSource(notDB); err == nil {
		t.Error("a non-SQLite file accepted")
	}
}

// TestInterruptedTriggersAreRestored: triggers left off by a run that stopped part way (recorded in the
// bookkeeping table) are put back by the next run, in their original order on SQLite.
func TestInterruptedTriggersAreRestored(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		e, tgt := newEngine(t, d)
		before := triggerState(t, tgt)
		if err := e.ensureBookkeeping(ctx); err != nil {
			t.Fatal(err)
		}
		if err := e.disableTriggers(ctx, FamilyS12a); err != nil { // and then the run "crashes"
			t.Fatal(err)
		}
		if after := triggerState(t, tgt); after == before {
			t.Fatal("fixture: triggers were not turned off")
		}
		mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
		if after := triggerState(t, tgt); after != before {
			t.Fatalf("triggers after recovery:\n%s\nwant:\n%s", after, before)
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM tether_migration_triggers`); got != 0 {
			t.Fatalf("%d trigger records left", got)
		}
	})
}

// triggerState renders the target's triggers: on SQLite each table's triggers in their firing (creation)
// order, which is what recreating them must keep; on Postgres each trigger's enabled flag.
func triggerState(t *testing.T, st *gostore.Store) string {
	t.Helper()
	q := `SELECT tbl_name || '.' || name FROM sqlite_master WHERE type = 'trigger' ORDER BY tbl_name, rowid`
	if st.Dialect().Name() == gostore.DriverPostgres {
		q = `SELECT tg.tgname || ':' || tg.tgenabled::text FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE NOT tg.tgisinternal AND n.nspname = current_schema() ORDER BY 1`
	}
	rows, err := st.Reader().QueryContext(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

// TestSourceCheckModes: the source check is reported, and an unknown mode is refused.
func TestSourceCheckModes(t *testing.T) {
	ctx := context.Background()
	for mode, want := range map[string]string{"": "integrity_check", "quick": "quick_check", "none": "none"} {
		e, _ := newEngine(t, gostore.DriverSQLite)
		rep := mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, SourceCheck: mode}))
		if rep.SourceCheck != want {
			t.Errorf("mode %q: source check %q, want %q", mode, rep.SourceCheck, want)
		}
	}
	e, _ := newEngine(t, gostore.DriverSQLite)
	if _, err := e.Run(ctx, Options{Phase: PhaseBulk, SourceCheck: "maybe"}); err == nil {
		t.Error("an unknown source check accepted")
	}
}
