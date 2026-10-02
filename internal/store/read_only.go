package store

import (
	"database/sql"
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
