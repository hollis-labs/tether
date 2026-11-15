// Package legacyguard keeps the new build from opening an old Tether database in place. It runs as
// boot.Options.Before["database"], before the database module opens and migrates the file.
//
// The old tree's database is recognizable by its custom migrator's schema_migrations table. Such a
// file is accepted only once migrate-from (slice S12) has finished copying it into the new layout and
// recorded state 'complete' in tether_migration_mark; a mark in any other state is an interrupted
// migration and is refused too. A missing file, an empty file and a database the new build created
// (goose's table only) are accepted.
package legacyguard

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"

	_ "modernc.org/sqlite" // registers the "sqlite" driver the database module also uses
)

const (
	// OldMigratorTable is the old tree's version table (internal/store/migrator.go).
	OldMigratorTable = "schema_migrations"
	// MarkTable is written by migrate-from (S12): one row, id 1, state 'running' then 'complete'.
	MarkTable = "tether_migration_mark"
	// MarkComplete is the state that admits a migrated database.
	MarkComplete = "complete"
)

var (
	// ErrLegacyLayout: an old Tether database with no migration mark.
	ErrLegacyLayout = errors.New("the database uses the old Tether layout")
	// ErrIncompleteMigration: a migration mark that is not 'complete'.
	ErrIncompleteMigration = errors.New("the database holds an unfinished migration")
)

// Check inspects the database file at path, read-only. It never creates or modifies the file.
func Check(path string) error { return check(path, false) }

// CheckMigrationTarget is Check for `tether migrate-from`: it admits a database whose migration is in
// progress (any mark state), since finishing it is the command's job, and still refuses an old-layout
// database, which migrate-from reads as a source and never writes into.
func CheckMigrationTarget(path string) error { return check(path, true) }

func check(path string, migrating bool) error {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.Size() == 0:
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	has := func(table string) (bool, error) {
		var n int
		err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		return n > 0, err
	}
	old, err := has(OldMigratorTable)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	marked, err := has(MarkTable)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if marked {
		var state string
		err := db.QueryRow(`SELECT state FROM ` + MarkTable + ` WHERE id = 1`).Scan(&state)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		if state == MarkComplete || migrating {
			return nil
		}
		return fmt.Errorf("%w (%s, state %q): rerun migrate-from to completion, or move the file aside and start again; see docs/conversion-notes.md",
			ErrIncompleteMigration, path, state)
	}
	if old {
		return fmt.Errorf("%w (%s): this build never opens the old database in place. Point TETHER_DATA_DIR at a new directory and run migrate-from; see docs/conversion-notes.md",
			ErrLegacyLayout, path)
	}
	return nil
}
