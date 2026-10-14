package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
)

// OpenReadOnly opens an existing state database without creating directories,
// changing its journal mode, or running migrations. Schema ownership stays
// with the daemon. The caller owns Close; all Store writes fail on this handle.
func OpenReadOnly(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// PendingMigrations reports embedded migration versions that schema_migrations
// does not record, without writing. A database with no schema_migrations table
// reports every embedded version as pending.
func PendingMigrations(db *sql.DB) ([]int, error) {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("sub-fs: %w", err)
	}
	migs, err := loadMigrations(sub)
	if err != nil {
		return nil, err
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&n); err != nil {
		return nil, err
	}
	applied := map[int]struct{}{}
	if n > 0 {
		if applied, err = loadAppliedVersions(db); err != nil {
			return nil, err
		}
	}
	var pending []int
	for _, m := range migs {
		if _, ok := applied[m.version]; !ok {
			pending = append(pending, m.version)
		}
	}
	return pending, nil
}
