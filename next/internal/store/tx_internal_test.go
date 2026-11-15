package store

import "testing"

func TestWriteTarget(t *testing.T) {
	for _, c := range []struct {
		q     string
		table string
		write bool
		bad   bool
	}{
		{q: `INSERT INTO settings (a) VALUES (?)`, table: "settings", write: true},
		{q: "  insert or ignore into \"Principals\" (a) values (1)", table: "principals", write: true},
		{q: `REPLACE INTO workers (id) VALUES (1)`, table: "workers", write: true},
		{q: `UPDATE sessions SET state = 'x' WHERE id IN (SELECT session_id FROM principals)`, table: "sessions", write: true},
		{q: `UPDATE OR ROLLBACK events SET pending = 0`, table: "events", write: true},
		{q: `DELETE FROM events WHERE id < ?`, table: "events", write: true},
		{q: "-- a comment\nDELETE FROM events", table: "events", write: true},
		{q: `WITH old AS (SELECT id FROM events WHERE at < ?) DELETE FROM events WHERE id IN (SELECT id FROM old)`, table: "events", write: true},
		{q: `INSERT INTO settings (k) VALUES ('x; DELETE FROM events')`, table: "settings", write: true},
		{q: `INSERT INTO settings (k) VALUES (1) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, table: "settings", write: true},
		{q: `SELECT * FROM events`},
		{q: `  select 1`},
		{q: `WITH x AS (SELECT 1) SELECT * FROM x`},
		{q: `VALUES (1)`},
		{q: `CREATE TABLE x (a)`, bad: true},
		{q: `PRAGMA foreign_keys = OFF`, bad: true},
		{q: `SELECT 1; DELETE FROM events`, bad: true},
		{q: `WITH x AS (DELETE FROM events RETURNING id) SELECT * FROM x`, bad: true},
	} {
		table, write, err := writeTarget(c.q)
		if (err != nil) != c.bad || table != c.table || write != c.write {
			t.Errorf("%q: got (%q, %v, %v), want (%q, %v, bad=%v)", c.q, table, write, err, c.table, c.write, c.bad)
		}
	}
}
