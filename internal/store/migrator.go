package store

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationPattern matches "NNNN_name.sql" where NNNN is one or more digits
// (zero-padded to 4 in current files, but the parser accepts any width).
var migrationPattern = regexp.MustCompile(`^(\d+)_([^/]+)\.sql$`)

// MigrateResult summarizes what happened during a Migrate call.
type MigrateResult struct {
	// Applied lists versions whose SQL ran against the DB, in order.
	Applied []int
	// Stamped lists versions recorded in schema_migrations without running
	// their SQL — used when adopting a pre-migrations (v0.0.1) database.
	Stamped []int
}

// Migrate applies all unapplied embedded migrations to db and returns a
// summary of what happened. It is idempotent: re-running on an already
// migrated database is a no-op.
func Migrate(db *sql.DB) (MigrateResult, error) {
	return MigrateWithProgress(db, "")
}

// MigrateWithProgress is Migrate that, when progressPath is set, keeps a
// MigrationProgress file there while each migration runs and removes it when
// done (CW-20261115-0046): a release tool can tell a daemon that is migrating
// from one that is down. Each migration's elapsed time is logged.
func MigrateWithProgress(db *sql.DB, progressPath string) (MigrateResult, error) {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return MigrateResult{}, fmt.Errorf("sub-fs: %w", err)
	}
	return migrateFSWithProgress(db, sub, progressPath)
}

// MigrationProgress is the migration a process is applying right now.
type MigrationProgress struct {
	Version   int       `json:"version"`
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
	PID       int       `json:"pid"`
	// Remaining are the versions still to apply after this one.
	Remaining []int `json:"remaining"`
}

// MigrationProgressPath is where the daemon keeps its progress file for the
// state database at dbPath.
func MigrationProgressPath(dbPath string) string { return dbPath + ".migrating" }

// ReadMigrationProgress reads the progress file for dbPath; ok is false when
// no migration is running, including when the file names a process that is no
// longer alive (a daemon that died mid-migration leaves a stale file).
func ReadMigrationProgress(dbPath string) (MigrationProgress, bool, error) {
	raw, err := os.ReadFile(MigrationProgressPath(dbPath)) //nolint:gosec // G304: the daemon's own state path
	if errors.Is(err, fs.ErrNotExist) {
		return MigrationProgress{}, false, nil
	}
	if err != nil {
		return MigrationProgress{}, false, err
	}
	var p MigrationProgress
	if err = json.Unmarshal(raw, &p); err != nil {
		return MigrationProgress{}, false, err
	}
	if !processAlive(p.PID) {
		return p, false, nil
	}
	return p, true, nil
}

// processAlive reports whether pid names a running process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// afterProgressWrite, when set by a test, runs once a migration's progress
// file is written and before the migration is applied.
var afterProgressWrite func()

// writeMigrationProgress writes p to path atomically and reports whether it did.
func writeMigrationProgress(path string, p MigrationProgress) bool {
	if path == "" {
		return false
	}
	raw, _ := json.Marshal(p)
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("store: write migration progress: %v", err)
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("store: write migration progress: %v", err)
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// migrateFS is the testable core — it accepts any fs.FS that lists
// migration files at its root.
func migrateFS(db *sql.DB, fsys fs.FS) (MigrateResult, error) {
	return migrateFSWithProgress(db, fsys, "")
}

// migrateFSWithProgress keeps the progress file only while it is applying a
// migration, and removes only a file it wrote itself: a process that opens the
// database while another is migrating it (a CLI beside the daemon) never erases
// the migrating process's signal (#592 review).
func migrateFSWithProgress(db *sql.DB, fsys fs.FS, progressPath string) (MigrateResult, error) {
	wrote := false
	defer func() {
		if wrote {
			_ = os.Remove(progressPath)
		}
	}()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
        version    INTEGER PRIMARY KEY,
        name       TEXT NOT NULL,
        applied_at TEXT NOT NULL
    )`); err != nil {
		return MigrateResult{}, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadAppliedVersions(db)
	if err != nil {
		return MigrateResult{}, err
	}

	migs, err := loadMigrations(fsys)
	if err != nil {
		return MigrateResult{}, err
	}

	var pending []int
	for _, m := range migs {
		if _, ok := applied[m.version]; !ok {
			pending = append(pending, m.version)
		}
	}
	var res MigrateResult
	for _, m := range migs {
		if _, ok := applied[m.version]; ok {
			continue
		}
		pending = pending[1:]
		// Adoption path: first migration against a pre-migrations DB.
		if m.version == 1 {
			adopted, err := hasLegacyTables(db)
			if err != nil {
				return res, err
			}
			if adopted {
				if err := recordMigration(db, m); err != nil {
					return res, err
				}
				log.Printf("store: adopted v0.0.1 database; stamped migration %04d_%s without re-running", m.version, m.name)
				res.Stamped = append(res.Stamped, m.version)
				continue
			}
		}
		start := time.Now()
		if writeMigrationProgress(progressPath, MigrationProgress{Version: m.version, Name: m.name, StartedAt: start.UTC(), PID: os.Getpid(), Remaining: append([]int{}, pending...)}) {
			wrote = true
		}
		if afterProgressWrite != nil {
			afterProgressWrite()
		}
		log.Printf("store: applying migration %04d_%s (%d more after it)", m.version, m.name, len(pending))
		if err := applyMigration(db, m); err != nil {
			return res, err
		}
		log.Printf("store: migration %04d_%s applied in %s", m.version, m.name, time.Since(start).Round(time.Millisecond))
		res.Applied = append(res.Applied, m.version)
	}
	return res, nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	seen := map[int]string{}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		matches := migrationPattern.FindStringSubmatch(e.Name())
		if matches == nil {
			return nil, fmt.Errorf("migration %q does not match NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("parse version from %q: %w", e.Name(), err)
		}
		if other, ok := seen[v]; ok {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", v, other, e.Name())
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: matches[2], sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func loadAppliedVersions(db *sql.DB) (map[int]struct{}, error) {
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("select schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[int]struct{}{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = struct{}{}
	}
	return out, rows.Err()
}

func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx for %d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(m.sql); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply %d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record %d_%s: %w", m.version, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %d_%s: %w", m.version, m.name, err)
	}
	return nil
}

func recordMigration(db *sql.DB, m migration) error {
	_, err := db.Exec(
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("stamp %d_%s: %w", m.version, m.name, err)
	}
	return nil
}

// hasLegacyTables reports whether the DB already contains the v0.0.1
// `sessions` table. Used to recognize a pre-migrations database so that
// 0001 can be stamped rather than re-run.
func hasLegacyTables(db *sql.DB) (bool, error) {
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "sessions",
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite_master lookup: %w", err)
	}
	return true, nil
}
