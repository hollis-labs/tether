// Package fabricstore persists native fabric records without activating them.
// It does not authenticate callers, verify artifact content, acquire leases or
// launch processes. Those policies belong to the enrollment/admission host.
package fabricstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

var (
	ErrConflict = errors.New("fabric record version conflict")
	ErrNotFound = errors.New("fabric record not found")
	ErrInvalid  = errors.New("invalid fabric record")
)

// Repository borrows an already migrated database; it never closes it.
type Repository struct{ db *sql.DB }

func New(db *sql.DB) *Repository { return &Repository{db: db} }

// Tx reserves SQLite's writer before reading references or versions. Separate
// daemon connections therefore cannot validate against the same stale snapshot.
// A callback must use Tx methods, not the Repository or the borrowed DB.
type Tx struct {
	conn *sql.Conn
	ctx  context.Context
}

func (r *Repository) Write(ctx context.Context, fn func(*Tx) error) error {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err = fn(&Tx{conn: conn, ctx: ctx}); err != nil {
		return err
	}
	// Cross-record invariants are checked at commit so callers may update both
	// actor and agent lifecycle in either order in one atomic transaction.
	var mismatches int
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM fabric_agents a
 LEFT JOIN fabric_actors p ON p.record_key=a.record_key
 WHERE p.record_key IS NULL
 OR json_extract(p.record_json,'$.kind') != 'agent'
 OR json_extract(p.record_json,'$.owner') != json_extract(a.record_json,'$.owner')
 OR json_extract(p.record_json,'$.lifecycle') != json_extract(a.record_json,'$.lifecycle')`).Scan(&mismatches); err != nil {
		return err
	}
	if mismatches != 0 {
		return invalid("actor and agent records disagree")
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// Record is a lossless mesh snapshot plus the host's optimistic concurrency
// version. Expected version zero means create; existing records start at one.
type Record[T any] struct {
	Value   T
	Version int64
}

type table string

const (
	definitions table = "fabric_definition_revisions"
	artifacts   table = "fabric_definition_artifacts"
	actors      table = "fabric_actors"
	agents      table = "fabric_agents"
	sessions    table = "fabric_sessions"
	instances   table = "fabric_instances"
	heads       table = "fabric_binding_heads"
	history     table = "fabric_binding_history"
	admissions  table = "fabric_admissions"
	candidates  table = "fabric_migration_candidates"
	receipts    table = "fabric_import_receipts"
	legacyRefs  table = "fabric_legacy_refs"
)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func read[T any](ctx context.Context, q queryer, t table, key string) (Record[T], error) {
	var result Record[T]
	var raw string
	// Table identifiers come only from package constants, never caller input.
	err := q.QueryRowContext(ctx, "SELECT record_json, record_version FROM "+string(t)+" WHERE record_key=?", key).Scan(&raw, &result.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(raw), &result.Value); err != nil {
		return result, fmt.Errorf("decode fabric record: %w", err)
	}
	return result, nil
}
func put[T any](tx *Tx, t table, key string, value T, expected int64) error {
	if key == "" || expected < 0 || expected == math.MaxInt64 {
		return ErrInvalid
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if expected == 0 {
		// #nosec G202 -- t is a private enum of fixed table names, never request data.
		result, err := tx.conn.ExecContext(tx.ctx, "INSERT INTO "+string(t)+"(record_key,record_version,record_json) VALUES (?,1,?) ON CONFLICT(record_key) DO NOTHING", key, string(raw))
		if err != nil {
			return err
		}
		return changed(result)
	}
	// #nosec G202 -- t is a private enum of fixed table names, never request data.
	result, err := tx.conn.ExecContext(tx.ctx, "UPDATE "+string(t)+" SET record_json=?,record_version=record_version+1 WHERE record_key=? AND record_version=?", string(raw), key, expected)
	if err != nil {
		return err
	}
	return changed(result)
}
func changed(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	return nil
}
func tuple(parts ...string) string { raw, _ := json.Marshal(parts); return string(raw) }
func invalid(reason string) error  { return fmt.Errorf("%w: %s", ErrInvalid, reason) }

var sqlReadOnly = sql.TxOptions{ReadOnly: true}
