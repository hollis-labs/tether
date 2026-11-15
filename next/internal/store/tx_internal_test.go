package store

import (
	"strings"
	"testing"
)

func TestWriteTargets(t *testing.T) {
	for _, c := range []struct {
		q      string
		tables string // comma-separated, "" for a read
		bad    bool
	}{
		{q: `INSERT INTO settings (a) VALUES (?)`, tables: "settings"},
		{q: "  insert or ignore into \"Principals\" (a) values (1)", tables: "principals"},
		{q: `REPLACE INTO workers (id) VALUES (1)`, tables: "workers"},
		{q: `UPDATE sessions SET state = 'x' WHERE id IN (SELECT session_id FROM principals)`, tables: "sessions"},
		{q: `UPDATE OR ROLLBACK events SET pending = 0`, tables: "events"},
		{q: `DELETE FROM events WHERE id < ?`, tables: "events"},
		{q: "-- a comment\nDELETE FROM events", tables: "events"},
		{q: `INSERT INTO settings (k) VALUES ('x; DELETE FROM events')`, tables: "settings"},
		{q: `INSERT INTO settings (k) VALUES (1) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, tables: "settings"},
		{q: `WITH old AS (SELECT id FROM events WHERE at < ?) DELETE FROM events WHERE id IN (SELECT id FROM old)`, tables: "events"},
		{q: `WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r WHERE n < 3) INSERT INTO settings (k) SELECT n FROM r`, tables: "settings"},
		{q: `WITH x AS (SELECT replace(k, 'a', 'b') AS k FROM settings) INSERT INTO settings (k) SELECT k FROM x ON CONFLICT (k) DO UPDATE SET v = 1`, tables: "settings"},
		// Data-modifying CTEs (Postgres): every target counts.
		{q: `WITH u AS (DELETE FROM messages RETURNING id) INSERT INTO events (event_id) SELECT id FROM u`, tables: "messages,events"},
		{q: `WITH u AS (UPDATE sessions SET state = 'x' RETURNING id) SELECT * FROM u`, tables: "sessions"},
		{q: `WITH a AS (INSERT INTO principals (x) VALUES (1) RETURNING id), b AS (DELETE FROM events RETURNING id) SELECT 1`, tables: "principals,events"},
		{q: `SELECT * FROM events`},
		{q: `  select id FROM sessions FOR UPDATE`},
		{q: `WITH x AS (SELECT id FROM sessions FOR NO KEY UPDATE) SELECT * FROM x`},
		{q: `WITH x AS (SELECT 1) SELECT * FROM x`},
		{q: `VALUES (1)`},
		{q: `CREATE TABLE x (a)`, bad: true},
		{q: `PRAGMA foreign_keys = OFF`, bad: true},
		{q: `SELECT 1; DELETE FROM events`, bad: true},
		{q: `/* c */ DELETE FROM events`, bad: true},
	} {
		got, err := writeTargets(c.q)
		if (err != nil) != c.bad || strings.Join(got, ",") != c.tables {
			t.Errorf("%q: got (%v, %v), want (%q, bad=%v)", c.q, got, err, c.tables, c.bad)
		}
	}
}
