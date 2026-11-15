package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/next/internal/legacyguard"
	"github.com/hollis-labs/tether/next/internal/migrate"
)

// oldCopy builds an old-layout database from the vendored old migrations, with a few messages.
func oldCopy(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "internal", "migrate", "testdata", "oldmigrations")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	path := filepath.Join(t.TempDir(), "old-copy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // test data
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (?, ?, 't')`, i+1, n); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := db.Exec(`INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES (?, 'notice', 'f', 't', 'c')`, id); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// coveredCopy is oldCopy without the tables of families not ported yet (and the old triggers that read
// them), so a final run can complete.
func coveredCopy(t *testing.T) string {
	t.Helper()
	path := oldCopy(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var drop []string
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if !migrate.Covers(n) {
			drop = append(drop, n)
		}
	}
	_ = rows.Close()
	trows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'trigger'`)
	if err != nil {
		t.Fatal(err)
	}
	var trigs [][2]string
	for trows.Next() {
		var n, q string
		if err := trows.Scan(&n, &q); err != nil {
			t.Fatal(err)
		}
		trigs = append(trigs, [2]string{n, q})
	}
	_ = trows.Close()
	for _, tg := range trigs {
		for _, d := range drop {
			if strings.Contains(tg[1], d) {
				if _, err := db.Exec(`DROP TRIGGER "` + tg[0] + `"`); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
	}
	for _, d := range drop {
		if _, err := db.Exec(`DROP TABLE "` + d + `"`); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestMigrateFromCommand: bulk, then serve is refused (the database is not complete), then final, after
// which the database is admitted.
func TestMigrateFromCommand(t *testing.T) {
	configure(t, freeAddr(t))
	old := coveredCopy(t)
	data := filepath.Join(t.TempDir(), "data")
	ctx := context.Background()
	report := filepath.Join(t.TempDir(), "report.json")
	if err := run(ctx, env(data), []string{"migrate-from", "--old-db", old, "--phase", "bulk", "--report", report}); err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if b, err := os.ReadFile(report); err != nil || !strings.Contains(string(b), `"mark": "bulk"`) {
		t.Fatalf("report: %v %.200s", err, b)
	}
	if err := run(ctx, env(data), []string{"serve"}); !errors.Is(err, legacyguard.ErrIncompleteMigration) {
		t.Fatalf("serve after bulk only: %v, want ErrIncompleteMigration", err)
	}
	if err := run(ctx, env(data), []string{"migrate-from", "--old-db", old}); err == nil || !strings.Contains(err.Error(), "--phase is required") {
		t.Fatalf("no --phase: %v, want it required", err)
	}
	if err := run(ctx, env(data), []string{"migrate-from", "--old-db", old, "--phase", "final"}); err != nil {
		t.Fatalf("final: %v", err)
	}
	if err := legacyguard.Check(filepath.Join(data, databaseFile)); err != nil {
		t.Fatalf("after final: %v", err)
	}
	// A complete database is never rewritten; --verify re-checks it.
	if err := run(ctx, env(data), []string{"migrate-from", "--old-db", old, "--phase", "final"}); !errors.Is(err, migrate.ErrTargetComplete) {
		t.Fatalf("final over a complete database: %v, want ErrTargetComplete", err)
	}
	if err := run(ctx, env(data), []string{"migrate-from", "--old-db", old, "--verify"}); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestMigrateFromRefusesALiveLookingSource: a source with a -wal file beside it is not a copy.
func TestMigrateFromRefusesALiveLookingSource(t *testing.T) {
	configure(t, freeAddr(t))
	old := oldCopy(t)
	if err := os.WriteFile(old+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), env(filepath.Join(t.TempDir(), "data")), []string{"migrate-from", "--old-db", old, "--phase", "bulk"})
	if !errors.Is(err, migrate.ErrLiveSource) {
		t.Fatalf("err = %v, want ErrLiveSource", err)
	}
}

// TestMigrateFromPrecheck: --precheck reports and changes nothing; it fails on a stuck launch.
func TestMigrateFromPrecheck(t *testing.T) {
	configure(t, freeAddr(t))
	old := oldCopy(t)
	data := filepath.Join(t.TempDir(), "data")
	if err := run(context.Background(), env(data), []string{"migrate-from", "--old-db", old, "--precheck"}); err != nil {
		t.Fatalf("precheck on a clean copy: %v", err)
	}
	db, err := sql.Open("sqlite", old)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sessions (id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, created_at, updated_at) VALUES ('s', 'l', 'p', 'a', 'pv', '/w', 'created', 't', 't')`,
		`INSERT INTO workspace_scratch_allocations (session_id, operation_id, session_digest, plan_digest, root, status) VALUES ('s', 'o', 'sd', 'pd', '/r', 'admitted')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	err = run(context.Background(), env(data), []string{"migrate-from", "--old-db", old, "--precheck"})
	if err == nil || !strings.Contains(err.Error(), "pre-check") {
		t.Fatalf("precheck with a stuck launch: %v", err)
	}
}

// TestMigrateFromFinalRefusesUncovered: on a full old copy (families not ported yet), a final run is refused
// and names the uncovered tables; a bulk run is fine.
func TestMigrateFromFinalRefusesUncovered(t *testing.T) {
	configure(t, freeAddr(t))
	old := oldCopy(t)
	data := filepath.Join(t.TempDir(), "data")
	if err := run(context.Background(), env(data), []string{"migrate-from", "--old-db", old, "--phase", "bulk"}); err != nil {
		t.Fatalf("bulk: %v", err)
	}
	err := run(context.Background(), env(data), []string{"migrate-from", "--old-db", old, "--phase", "final"})
	if !errors.Is(err, migrate.ErrIncomplete) || !strings.Contains(err.Error(), "registry_entries") {
		t.Fatalf("final on a full copy: %v, want ErrIncomplete naming registry_entries", err)
	}
}
