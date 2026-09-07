package e2e

// upgrade_test.go — T11 "upgrades from current fixture data" scenario.
// Unit-level migration coverage already exists (internal/store's own
// migrator_test.go: TestMigrate_AdoptsExistingV001 and friends) but only
// ever exercises store.Migrate() directly against an in-memory/temp
// *sql.DB, never the actual production startup path a real `mux daemon
// run` process takes. This seeds a real SQLite file with the exact
// pre-migrations v0.0.1 schema (mirroring migrator_test.go's own fixture)
// PLUS a real row, places it at the isolated fixture's real state-db
// path BEFORE the daemon ever starts, boots the real binary against it,
// and confirms both that startup succeeds (every migration in
// internal/store/migrations/ applied cleanly, for real) and that the
// pre-existing row is still there afterward.

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

const legacyV001Schema = `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY, launch_id TEXT NOT NULL, project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL, provider_id TEXT NOT NULL, workspace TEXT NOT NULL,
    state TEXT NOT NULL, pid INTEGER, exit_code INTEGER,
    created_at TEXT NOT NULL, updated_at TEXT NOT NULL, ended_at TEXT);
CREATE TABLE launch_plans (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    plan_json TEXT NOT NULL);
CREATE TABLE events (
    id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
    at TEXT NOT NULL, kind TEXT NOT NULL, payload TEXT);`

func TestUpgrade_RealDaemonMigratesLegacyFixtureDataOnBoot(t *testing.T) {
	// Build the fixture DB file BEFORE starting a daemon against it --
	// StartFixtureDaemon can't be used here since it boots immediately;
	// construct the isolated state root by hand instead, matching what
	// the harness does internally.
	stateRoot, err := os.MkdirTemp("", "te2eup")
	if err != nil {
		t.Fatalf("mkdir state root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })

	dbDir := filepath.Join(stateRoot, "state")
	if err := os.MkdirAll(dbDir, 0o750); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	dbPath := filepath.Join(dbDir, "tether.db")

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if _, err := rawDB.Exec(legacyV001Schema); err != nil {
		t.Fatalf("seed legacy v0.0.1 schema: %v", err)
	}
	const legacySessionID = "sess-legacy-e2e-fixture"
	if _, err := rawDB.Exec(
		`INSERT INTO sessions (id, launch_id, project_id, agent_id, provider_id, workspace, state, created_at, updated_at)
		 VALUES (?, 'launch-x', 'proj-x', 'agent-x', 'provider-x', '/tmp/ws', 'stopped', '2020-01-01T00:00:00Z', '2020-01-01T00:00:00Z')`,
		legacySessionID,
	); err != nil {
		t.Fatalf("seed legacy session row: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	d := startFixtureDaemonAt(t, stateRoot)

	// Boot succeeded (StartFixtureDaemon/startFixtureDaemonAt would have
	// t.Fatal'd on health-check failure) -- every migration file applied
	// cleanly against real pre-existing legacy data, in the real binary.
	if err := d.Client().Ping(t.Context()); err != nil {
		t.Fatalf("ping after migrating legacy fixture: %v", err)
	}

	d.Stop()

	// Re-open the now-migrated file directly to confirm the legacy row
	// survived migration rather than being dropped or orphaned.
	verifyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	defer verifyDB.Close()

	var gotID, gotState string
	if err := verifyDB.QueryRow(`SELECT id, state FROM sessions WHERE id = ?`, legacySessionID).Scan(&gotID, &gotState); err != nil {
		t.Fatalf("legacy session row missing after real-daemon migration: %v", err)
	}
	if gotID != legacySessionID || gotState != "stopped" {
		t.Fatalf("migrated row = (%q, %q), want (%q, %q)", gotID, gotState, legacySessionID, "stopped")
	}

	var migCount int
	if err := verifyDB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migCount); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if migCount == 0 {
		t.Fatal("schema_migrations is empty after boot -- migrations did not actually run")
	}
}
