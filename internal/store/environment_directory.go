package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	tether "github.com/hollis-labs/substrate/mesh/tetherclient"
	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var _ directory.Storage = (*Store)(nil)

func directorySQLError(err error) error {
	var sq *sqlite.Error
	if errors.As(err, &sq) && sq.Code()&0xff == sqlite3.SQLITE_CONSTRAINT {
		return directory.ErrConflict
	}
	return err
}

type directoryScanner interface{ Scan(...any) error }

func scanEnvironment(row directoryScanner) (directory.Record, error) {
	var body string
	if err := row.Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return directory.Record{}, directory.ErrNotFound
		}
		return directory.Record{}, err
	}
	var record directory.Record
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		return directory.Record{}, errors.New("environment directory: invalid stored record")
	}
	return record, nil
}

func (s *Store) GetEnvironment(ctx context.Context, id string) (directory.Record, error) {
	return scanEnvironment(s.db.QueryRowContext(ctx, `SELECT record_json FROM environment_directory WHERE environment_id=?`, id))
}
func (s *Store) EnvironmentByAuthority(ctx context.Context, a string) (directory.Record, error) {
	return scanEnvironment(s.db.QueryRowContext(ctx, `SELECT record_json FROM environment_directory WHERE authority=?`, a))
}
func (s *Store) ListEnvironments(ctx context.Context) ([]directory.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record_json FROM environment_directory ORDER BY authority`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []directory.Record{}
	for rows.Next() {
		r, e := scanEnvironment(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RegisterEnvironment rechecks immutable bindings and homes inside one write
// transaction. UNIQUE, not disabled foreign keys, enforces concurrent ownership.
func (s *Store) RegisterEnvironment(ctx context.Context, r directory.Record) (directory.Record, error) {
	normalized, err := directory.Normalize(r.Registration)
	if err != nil {
		return directory.Record{}, err
	}
	r.Registration = normalized
	if r.State != "reachable" || r.Protocol != tether.EnvironmentProtocol || r.LastSeen == nil || r.RevocationPending {
		return directory.Record{}, directory.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return directory.Record{}, err
	}
	defer tx.Rollback()
	old, e := scanEnvironment(tx.QueryRowContext(ctx, `SELECT record_json FROM environment_directory WHERE environment_id=?`, r.EnvironmentID))
	if e == nil {
		if old.State == "retired" {
			return directory.Record{}, directory.ErrRetired
		}
		if !directory.SameBinding(old.Registration, r.Registration) {
			return directory.Record{}, directory.ErrConflict
		}
		r.Label = old.Label
	} else if !errors.Is(e, directory.ErrNotFound) {
		return directory.Record{}, e
	}
	body, err := json.Marshal(r)
	if err != nil || len(body) > 256<<10 {
		return directory.Record{}, directory.ErrInvalid
	}
	if errors.Is(e, directory.ErrNotFound) {
		_, err = tx.ExecContext(ctx, `INSERT INTO environment_directory(environment_id,authority,record_json) VALUES(?,?,?)`, r.EnvironmentID, r.Authority, string(body))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE environment_directory SET record_json=? WHERE environment_id=?`, string(body), r.EnvironmentID)
	}
	if err != nil {
		return directory.Record{}, directorySQLError(err)
	}
	for _, h := range r.Homes {
		var homeID, authority, mode string
		e = tx.QueryRowContext(ctx, `SELECT environment_id,authority,management_mode FROM environment_agent_homes WHERE urn=?`, h.URN).Scan(&homeID, &authority, &mode)
		if e == nil {
			if homeID != r.EnvironmentID || authority != h.Authority || mode != string(h.ManagementMode) {
				return directory.Record{}, directory.ErrConflict
			}
			continue
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return directory.Record{}, e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO environment_agent_homes(urn,environment_id,authority,management_mode) VALUES(?,?,?,?)`, h.URN, r.EnvironmentID, h.Authority, string(h.ManagementMode)); err != nil {
			return directory.Record{}, directorySQLError(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return directory.Record{}, directorySQLError(err)
	}
	return r, nil
}

// mutateEnvironment serializes state changes and keeps identity-bearing columns
// intact. A failed commit never publishes a route or a claimed revoke success.
func (s *Store) mutateEnvironment(ctx context.Context, id string, fn func(*directory.Record) error) (directory.Record, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return directory.Record{}, err
	}
	defer tx.Rollback()
	r, err := scanEnvironment(tx.QueryRowContext(ctx, `SELECT record_json FROM environment_directory WHERE environment_id=?`, id))
	if err != nil {
		return directory.Record{}, err
	}
	if err = fn(&r); err != nil {
		return directory.Record{}, err
	}
	body, err := json.Marshal(r)
	if err != nil || len(body) > 256<<10 {
		return directory.Record{}, directory.ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, `UPDATE environment_directory SET record_json=? WHERE environment_id=?`, string(body), id); err != nil {
		return directory.Record{}, err
	}
	if err = tx.Commit(); err != nil {
		return directory.Record{}, err
	}
	return r, nil
}
func (s *Store) RenameEnvironment(ctx context.Context, id, label string) (directory.Record, error) {
	if len(label) > 256 || strings.ContainsAny(label, "\x00\r\n") {
		return directory.Record{}, directory.ErrInvalid
	}
	return s.mutateEnvironment(ctx, id, func(r *directory.Record) error { r.Label = label; return nil })
}
func (s *Store) RetireEnvironment(ctx context.Context, id string) (directory.Record, bool, error) {
	first := false
	r, err := s.mutateEnvironment(ctx, id, func(r *directory.Record) error {
		if r.State != "retired" {
			first = true
			r.State = "retired"
			r.RevocationPending = true
		}
		return nil
	})
	return r, first, err
}
func (s *Store) CompleteEnvironmentRevocation(ctx context.Context, id string) (directory.Record, error) {
	return s.mutateEnvironment(ctx, id, func(r *directory.Record) error {
		if r.State != "retired" {
			return directory.ErrInvalid
		}
		r.RevocationPending = false
		return nil
	})
}
func (s *Store) ObserveEnvironment(ctx context.Context, id, state string, d *tether.EnvironmentDescriptor, seen *time.Time) (directory.Record, error) {
	switch state {
	case "enrolled", "reachable", "unreachable", "incompatible":
	default:
		return directory.Record{}, directory.ErrInvalid
	}
	if d != nil && (d.EnvironmentID != id || d.Protocol != tether.EnvironmentProtocol || state != "reachable" || seen == nil) {
		return directory.Record{}, directory.ErrInvalid
	}
	if d == nil && (state == "reachable" || seen != nil) {
		return directory.Record{}, directory.ErrInvalid
	}
	return s.mutateEnvironment(ctx, id, func(r *directory.Record) error {
		if r.State == "retired" {
			return directory.ErrRetired
		}
		r.State = state
		if d != nil {
			r.Protocol = d.Protocol
			r.ServerVersion = d.ServerVersion
			r.Capabilities = d.Capabilities
			r.LastSeen = seen
		}
		return nil
	})
}
