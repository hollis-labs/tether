package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// oldFixture builds an old-layout database at migration 0101 holding rows in every S12a table, with the
// shapes a live database has: rowid gaps in messages, an AUTOINCREMENT mark above the highest id, an
// orphan row, CURRENT_TIMESTAMP text, a BLOB payload, NULLs and non-ASCII text. It returns the file path.
func oldFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	applyOld(t, db)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO event_identity (singleton, environment_id, cutover, next_legacy_id, remaining) VALUES (1, 'f64b485f-7f8d-441b-89d3-378b70e81352', 40, 0, 0)`)
	exec(`INSERT INTO event_retention_policies (id, policy_json, created_at) VALUES ('p1', '{"days":90}', '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO event_retention_admission (kind, retention_class, policy_id) VALUES ('session.output', 'R-debug', 'p1')`)
	exec(`INSERT INTO execution_policy (id, policy_json, updated_at) VALUES (1, '{"paused":false}', '2026-01-01T00:00:00.123456789Z')`)
	for i := 1; i <= 3; i++ {
		exec(`INSERT INTO fabric_actors (record_key, record_version, record_json) VALUES (?, 1, '{}')`, fmt.Sprintf("actor-%d", i))
	}
	exec(`INSERT INTO fabric_agents (record_key, record_version, record_json) VALUES ('agent-1', 2, '{"actor":"actor-1"}')`)
	exec(`INSERT INTO fabric_definition_artifacts (record_key, record_version, record_json) VALUES ('art', 1, '{}')`)
	exec(`INSERT INTO fabric_definition_revisions (record_key, record_version, record_json) VALUES ('rev', 1, '{}')`)
	for i := 0; i < 25; i++ {
		exec(`INSERT INTO logical_agents (id, role, name, created_at, updated_at) VALUES (?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			fmt.Sprintf("agent-%02d", i), nullIf(i%3 == 0, "builder"), "Agent é ✓ "+fmt.Sprint(i))
	}
	exec(`INSERT INTO pairing_grants (id, code_hash, label, scopes_json, created_by, created_at, expires_at) VALUES ('g1', ?, 'l', '[]', 'op', 't', 't')`, strings.Repeat("a", 64))
	for i := 1; i <= 5; i++ {
		exec(`INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, addresses_json, created_at) VALUES (?, 'service', 'd', ?, '[]', '[]', 't')`,
			fmt.Sprintf("svc-%d", i), fmt.Sprintf("%064d", i))
	}
	exec(`DELETE FROM principals WHERE id IN (2, 5)`) // sqlite_sequence stays at 5, above max(id) 4
	exec(`INSERT INTO settings (scope, scope_id, key, value_json) VALUES ('global', '', 'k', '1')`) // CURRENT_TIMESTAMP default
	exec(`INSERT INTO settings (scope, scope_id, key, value_json, updated_at) VALUES ('project', 'p', 'k', '"x"', '2026-01-02 03:04:05')`)
	exec(`INSERT INTO team_definitions (team_id, version, payload) VALUES ('team', 1, ?)`, []byte(`{"members":[]}`))
	exec(`INSERT INTO team_definitions (team_id, version, payload) VALUES ('team', 2, '{"stored":"as text"}')`)
	exec(`INSERT INTO workers (id, label, device_id, platform, last_seen, created_at) VALUES ('w1', 'l', 'd1', 'linux', 't', 't')`)
	exec(`INSERT INTO worker_mail_grants (worker_id, urn) VALUES ('w1', 'msg://agent/a')`)
	exec(`INSERT INTO worker_mail_grants (worker_id, urn) VALUES ('gone', 'msg://agent/b')`) // an orphan, copied as is
	for i := 1; i <= 120; i++ {
		exec(`INSERT INTO messages (id, kind, from_urn, to_urn, payload, metadata, created_at) VALUES (?, 'notice', 'f', 't', ?, NULL, ?)`,
			fmt.Sprintf("m-%03d", i), fmt.Sprintf("body %d ✓", i), fmt.Sprintf("2026-01-01T00:%02d:00Z", i%60))
	}
	exec(`DELETE FROM messages WHERE id IN ('m-007', 'm-050', 'm-120')`) // rowid gaps, including the newest
	return path
}

// applyOld runs the vendored old migrations on db (the old migrator's way; see buildOld).
func applyOld(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range readOld(t) {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("old migration %04d: %v", m.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (?, ?, 't')`, m.version, m.name); err != nil {
			t.Fatal(err)
		}
	}
}

func nullIf(null bool, s string) any {
	if null {
		return nil
	}
	return s
}

func newEngine(t *testing.T, d gostore.Driver) (*Engine, *gostore.Store) {
	t.Helper()
	path := oldFixture(t)
	src, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	tgt := storetest.Raw(t, d)
	return New(src, path, tgt), tgt
}

func eachTarget(t *testing.T, fn func(t *testing.T, d gostore.Driver)) {
	for _, d := range []gostore.Driver{gostore.DriverSQLite, gostore.DriverPostgres} {
		t.Run(string(d), func(t *testing.T) { fn(t, d) })
	}
}

// mustOK returns a check for a run's results: no error and no problem.
func mustOK(t *testing.T) func(*Report, error) *Report {
	return func(rep *Report, err error) *Report {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if !rep.OK() {
			t.Fatalf("problems: %v", rep.Problems)
		}
		return rep
	}
}

func queryInt(t *testing.T, st *gostore.Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := st.Reader().QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestMigrateFromCopiesAndVerifies: a bulk run copies every S12a table and verifies it; a final run over
// the copied database changes nothing and marks it complete; row identity is carried.
func TestMigrateFromCopiesAndVerifies(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		e, tgt := newEngine(t, d)
		rep := mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 7}))
		if rep.Mark != MarkBulk || !rep.SourceUnchanged || rep.MessagesSeq != "ok" {
			t.Fatalf("bulk: mark %q, unchanged %v, seq %q", rep.Mark, rep.SourceUnchanged, rep.MessagesSeq)
		}
		for table, c := range rep.Counts {
			if c[0] != c[1] {
				t.Errorf("%s: counts %v", table, c)
			}
		}
		if rep.Counts["messages"][0] != 117 || rep.Counts["logical_agents"][0] != 25 {
			t.Fatalf("fixture counts: %v", rep.Counts)
		}
		// Carried identity: messages.seq is the old rowid, gaps included; principals keep their ids.
		if got := queryInt(t, tgt, `SELECT seq FROM messages WHERE id = 'm-119'`); got != 119 {
			t.Errorf("messages m-119 seq = %d, want 119", got)
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM principals WHERE id IN (1, 3, 4)`); got != 3 {
			t.Errorf("principal ids not carried")
		}
		// The copy fired no trigger: no journal rows, and the triggers are back afterwards.
		if got := queryInt(t, tgt, `SELECT count(*) FROM message_event_journal`); got != 0 {
			t.Errorf("the copy wrote %d journal rows", got)
		}
		if d == gostore.DriverSQLite {
			if got := queryInt(t, tgt, `SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND tbl_name = 'messages'`); got < 8 {
				t.Errorf("messages triggers after the copy: %d", got)
			}
			if got := queryInt(t, tgt, `SELECT seq FROM sqlite_sequence WHERE name = 'principals'`); got != 5 {
				t.Errorf("principals sqlite_sequence = %d, want 5 (carried, above max(id) 4)", got)
			}
			if fk := rep.ForeignKeys["worker_mail_grants"]; fk != [2]int64{1, 1} {
				t.Errorf("worker_mail_grants FK violations = %v, want the source's one orphan, kept", fk)
			}
			if rep.Integrity != "ok" {
				t.Errorf("integrity %q", rep.Integrity)
			}
			if got := queryInt(t, tgt, `SELECT rowid FROM messages WHERE id = 'm-119'`); got != 119 {
				t.Errorf("messages rowid not carried on SQLite: %d", got)
			}
		}

		final := mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 7}))
		for _, c := range final.Copy {
			if c.Recopied != 0 {
				t.Errorf("final over an unchanged copy re-copied %d ranges of %s", c.Recopied, c.Table)
			}
		}
		var state string
		if err := tgt.Reader().QueryRowContext(ctx, `SELECT state FROM tether_migration_mark WHERE id = 1`).Scan(&state); err != nil || state != MarkComplete {
			t.Fatalf("mark = %q, %v", state, err)
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM tether_migration_tables`); got != int64(len(FamilyS12a)) {
			t.Errorf("per-table marks: %d", got)
		}
		// After the migration, a new principal continues past the carried mark (5), never reissuing an id.
		if _, err := tgt.ExecContext(ctx, `INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, addresses_json, created_at)
			VALUES ('new', 'service', 'd', ?, '[]', '[]', 't')`, strings.Repeat("f", 64)); err != nil {
			t.Fatal(err)
		}
		if got := queryInt(t, tgt, `SELECT id FROM principals WHERE principal_id = 'new'`); got != 6 {
			t.Errorf("new principal id = %d, want 6", got)
		}
	})
}

// TestMigrateFromFindsAndRepairsDrift: a changed, an extra and a missing target row are each reported by
// the key range that holds them; a final run re-copies exactly those ranges and verifies clean.
func TestMigrateFromFindsAndRepairsDrift(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		e, tgt := newEngine(t, d)
		mustOK(t)(e.Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))
		for _, q := range []string{
			`UPDATE messages SET payload = 'tampered' WHERE id = 'm-042'`,
			`DELETE FROM logical_agents WHERE id = 'agent-13'`,
			`INSERT INTO settings (scope, scope_id, key, value_json, updated_at) VALUES ('user', 'u', 'extra', '1', 't')`,
		} {
			if _, err := tgt.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		rep, err := e.Verify(ctx, Options{Batch: 10})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{"messages": false, "logical_agents": false, "settings": false}
		for _, tr := range rep.Verify {
			for _, m := range tr.Mismatched {
				switch tr.Table {
				case "messages":
					want["messages"] = want["messages"] || (m.Lo < "m-042" && (m.Hi == "" || m.Hi >= "m-042"))
				case "logical_agents":
					want["logical_agents"] = want["logical_agents"] || (m.Lo < "agent-13" && (m.Hi == "" || m.Hi >= "agent-13"))
				case "settings":
					want["settings"] = true
				default:
					t.Errorf("unexpected mismatch in %s: %+v", tr.Table, m)
				}
			}
		}
		for table, found := range want {
			if !found {
				t.Errorf("%s drift not reported by its range: %+v", table, rep.Verify)
			}
		}
		if rep.OK() {
			t.Fatal("verification passed over drift")
		}
		final := mustOK(t)(e.Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
		recopied := 0
		for _, c := range final.Copy {
			recopied += c.Recopied
		}
		if recopied != 3 {
			t.Errorf("final re-copied %d ranges, want 3", recopied)
		}
		var payload string
		if err := tgt.Reader().QueryRowContext(ctx, `SELECT payload FROM messages WHERE id = 'm-042'`).Scan(&payload); err != nil || payload != "body 42 ✓" {
			t.Errorf("m-042 after repair: %q %v", payload, err)
		}
	})
}

// TestMigrateFromRefusesShapeMismatch: a source column the target lacks stops the run before any write.
func TestMigrateFromRefusesShapeMismatch(t *testing.T) {
	path := oldFixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE workers ADD COLUMN retired_at TEXT`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	src, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	tgt := storetest.Raw(t, gostore.DriverSQLite)
	_, err = New(src, path, tgt).Run(context.Background(), Options{Phase: PhaseBulk})
	if err == nil || !strings.Contains(err.Error(), "workers.retired_at") {
		t.Fatalf("err = %v, want the unmapped source column named", err)
	}
	if got := queryInt(t, tgt, `SELECT count(*) FROM messages`); got != 0 {
		t.Fatalf("rows were written before the refusal: %d", got)
	}
}

// TestOpenSourceRefusesANewDatabase: migrate-from reads old-layout databases only.
func TestOpenSourceRefusesANewDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE goose_db_version (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := OpenSource(path); err == nil {
		t.Fatal("a new-layout database accepted as a source")
	}
}
