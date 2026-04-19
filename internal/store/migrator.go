package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationPattern matches "NNNN_name.sql" where NNNN is one or more digits
// (zero-padded to 4 in current files, but the parser accepts any width).
var migrationPattern = regexp.MustCompile(`^(\d+)_([^/]+)\.sql$`)

// MigrateResult summarises what happened during a Migrate call.
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
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return MigrateResult{}, fmt.Errorf("sub-fs: %w", err)
	}
	return migrateFS(db, sub)
}

// migrateFS is the testable core — it accepts any fs.FS that lists
// migration files at its root.
func migrateFS(db *sql.DB, fsys fs.FS) (MigrateResult, error) {
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

	var res MigrateResult
	for _, m := range migs {
		if _, ok := applied[m.version]; ok {
			continue
		}
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
		if err := applyMigration(db, m); err != nil {
			return res, err
		}
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
// `sessions` table. Used to recognise a pre-migrations database so that
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
