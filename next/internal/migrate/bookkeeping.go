package migrate

import (
	"context"
	"fmt"
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// The engine's own tables (class A). They are created by migrate-from, never by the goose migrations: the
// S0 guard reads an existing tether_migration_mark as "this database was migrated", so a fresh database
// must not have one.
//
//	tether_migration_mark      one row: state (running, bulk, complete), phase, source_sha256, times
//	tether_migration_tables    one row per copied table: phase, rows, whole-table digest, source_sha256
//	tether_migration_triggers  the SQLite triggers dropped for the copy, until they are recreated
var bookkeepingDDL = []string{
	`CREATE TABLE IF NOT EXISTS tether_migration_mark (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		state TEXT NOT NULL,
		phase TEXT NOT NULL,
		source_sha256 TEXT NOT NULL,
		started_at TEXT NOT NULL,
		completed_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS tether_migration_tables (
		table_name TEXT PRIMARY KEY,
		phase TEXT NOT NULL,
		rows BIGINT NOT NULL,
		digest TEXT NOT NULL,
		source_sha256 TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS tether_migration_triggers (
		name TEXT PRIMARY KEY,
		table_name TEXT NOT NULL,
		sql TEXT NOT NULL
	)`,
}

func (e *Engine) sqlite() bool { return e.raw.Dialect().Name() == gostore.DriverSQLite }

func (e *Engine) ensureBookkeeping(ctx context.Context) error {
	for _, q := range bookkeepingDDL {
		if _, err := e.raw.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("create bookkeeping: %w", err)
		}
	}
	// A previous run that stopped mid-copy left its SQLite triggers saved here: put them back first.
	return e.enableTriggers(ctx)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (e *Engine) setMark(ctx context.Context, state string, phase Phase, sum string, done bool) error {
	var completed any
	if done {
		completed = now()
	}
	return e.stores.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO tether_migration_mark (id, state, phase, source_sha256, started_at, completed_at)
			VALUES (1, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET state = excluded.state, phase = excluded.phase,
				source_sha256 = excluded.source_sha256, completed_at = excluded.completed_at`,
			state, string(phase), sum, now(), completed)
		return err
	})
}

func (e *Engine) recordTables(ctx context.Context, phase Phase, sum string, rep *Report) error {
	return e.stores.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		for _, t := range rep.Verify {
			if _, err := tx.ExecContext(ctx, `INSERT INTO tether_migration_tables (table_name, phase, rows, digest, source_sha256, updated_at)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (table_name) DO UPDATE SET phase = excluded.phase, rows = excluded.rows, digest = excluded.digest,
					source_sha256 = excluded.source_sha256, updated_at = excluded.updated_at`,
				t.Table, string(phase), t.Rows, t.Digest, sum, now()); err != nil {
				return err
			}
		}
		return nil
	})
}

// disableTriggers turns off the target's triggers on the copied tables. SQLite has no switch: each
// trigger's DDL is saved, then the trigger is dropped, and enableTriggers recreates it. Postgres disables
// the user triggers per table.
func (e *Engine) disableTriggers(ctx context.Context, tables []string) error {
	if !e.sqlite() {
		for _, t := range tables {
			if _, err := e.raw.ExecContext(ctx, `ALTER TABLE `+quote(t)+` DISABLE TRIGGER USER`); err != nil {
				return fmt.Errorf("disable triggers on %s: %w", t, err)
			}
		}
		e.pgDisabled = tables
		return nil
	}
	in := "'" + strings.Join(tables, "', '") + "'"
	rows, err := e.raw.Reader().QueryContext(ctx, `SELECT name, tbl_name, sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name IN (`+in+`)`)
	if err != nil {
		return err
	}
	type trig struct{ name, table, sql string }
	var ts []trig
	for rows.Next() {
		var t trig
		if err := rows.Scan(&t.name, &t.table, &t.sql); err != nil {
			_ = rows.Close()
			return err
		}
		ts = append(ts, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, t := range ts {
		if _, err := e.raw.ExecContext(ctx, `INSERT INTO tether_migration_triggers (name, table_name, sql) VALUES (?, ?, ?)`, t.name, t.table, t.sql); err != nil {
			return fmt.Errorf("save trigger %s: %w", t.name, err)
		}
		if _, err := e.raw.ExecContext(ctx, `DROP TRIGGER `+quote(t.name)); err != nil {
			return fmt.Errorf("drop trigger %s: %w", t.name, err)
		}
	}
	return nil
}

// enableTriggers recreates every saved SQLite trigger and clears the list, or re-enables the Postgres
// triggers this run disabled.
func (e *Engine) enableTriggers(ctx context.Context) error {
	if !e.sqlite() {
		for _, t := range e.pgDisabled {
			if _, err := e.raw.ExecContext(ctx, `ALTER TABLE `+quote(t)+` ENABLE TRIGGER USER`); err != nil {
				return fmt.Errorf("enable triggers on %s: %w", t, err)
			}
		}
		e.pgDisabled = nil
		return nil
	}
	rows, err := e.raw.Reader().QueryContext(ctx, `SELECT name, sql FROM tether_migration_triggers ORDER BY name`)
	if err != nil {
		return err
	}
	var saved [][2]string
	for rows.Next() {
		var t [2]string
		if err := rows.Scan(&t[0], &t[1]); err != nil {
			_ = rows.Close()
			return err
		}
		saved = append(saved, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, t := range saved {
		var n int
		if err := e.raw.Reader().QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, t[0]).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := e.raw.ExecContext(ctx, t[1]); err != nil {
				return fmt.Errorf("recreate trigger %s: %w", t[0], err)
			}
		}
		if _, err := e.raw.ExecContext(ctx, `DELETE FROM tether_migration_triggers WHERE name = ?`, t[0]); err != nil {
			return err
		}
	}
	return nil
}

// carrySequences carries the source's AUTOINCREMENT high-water marks (sqlite_sequence), so no id a
// client holds is ever reissued. On Postgres each identity column is set past the larger of the
// source's mark and the copied rows' maximum.
func (e *Engine) carrySequences(ctx context.Context, tables []string) error {
	srcSeq := map[string]int64{}
	rows, err := e.src.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			_ = rows.Close()
			return err
		}
		srcSeq[name] = seq
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, t := range tables {
		if e.sqlite() {
			seq, ok := srcSeq[t]
			if !ok {
				continue
			}
			if _, err := e.raw.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = ?`, t); err != nil {
				return err
			}
			if _, err := e.raw.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)`, t, seq); err != nil {
				return err
			}
			continue
		}
		cols, err := e.raw.Reader().QueryContext(ctx, `SELECT column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ? AND is_identity = 'YES'`, t)
		if err != nil {
			return err
		}
		var idCols []string
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				_ = cols.Close()
				return err
			}
			idCols = append(idCols, c)
		}
		if err := cols.Close(); err != nil {
			return err
		}
		for _, c := range idCols {
			var maxv int64
			if err := e.raw.Reader().QueryRowContext(ctx, `SELECT COALESCE(MAX(`+quote(c)+`), 0) FROM `+quote(t)).Scan(&maxv); err != nil {
				return err
			}
			v := max(maxv, srcSeq[t])
			if v < 1 {
				continue
			}
			if _, err := e.raw.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence(?, ?), ?, true)`, quote(t), c, v); err != nil {
				return fmt.Errorf("set %s.%s identity: %w", t, c, err)
			}
		}
	}
	return nil
}
