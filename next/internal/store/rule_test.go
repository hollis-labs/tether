package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

const (
	insertSetting = `INSERT INTO settings (scope, scope_id, key, value_json) VALUES ('global', '', ?, '1')`
	insertEvent   = `INSERT INTO events (at, kind, event_id) VALUES ('t', 'k', ?)`
	insertMessage = `INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES (?, 'notice', 'f', 't', 'c')`
)

func count(t *testing.T, h *store.Handle, table string) int {
	t.Helper()
	var n int
	if err := h.Reader().QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWriteOwnClass: each handle writes its own class's tables.
func TestWriteOwnClass(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		for _, w := range []struct {
			h     *store.Handle
			q     string
			table string
		}{{s.A(), insertSetting, "settings"}, {s.B(), insertEvent, "events"}, {s.C(), insertMessage, "messages"}} {
			if err := w.h.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := tx.ExecContext(ctx, w.q, "x")
				return err
			}); err != nil {
				t.Fatalf("class %s writing %s: %v", w.h.Class(), w.table, err)
			}
			if n := count(t, w.h, w.table); n != 1 {
				t.Fatalf("%s holds %d rows, want 1", w.table, n)
			}
		}
	})
}

// TestCrossClassWriteRefused is the S2 rule (plan 13.1(4)): a class's transaction cannot write another
// class's table, through Exec, Query or QueryRow, and the refusal rolls the whole transaction back.
func TestCrossClassWriteRefused(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		calls := map[string]func(ctx context.Context, tx store.Tx) error{
			"exec": func(ctx context.Context, tx store.Tx) error {
				_, err := tx.ExecContext(ctx, insertEvent, "e1")
				return err
			},
			"query": func(ctx context.Context, tx store.Tx) error {
				rows, err := tx.QueryContext(ctx, insertEvent+` RETURNING id`, "e2")
				if err == nil {
					_ = rows.Close()
				}
				return err
			},
			"query row": func(ctx context.Context, tx store.Tx) error {
				var id int64
				return tx.QueryRowContext(ctx, insertEvent+` RETURNING id`, "e3").Scan(&id)
			},
		}
		for name, call := range calls {
			err := s.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
				if _, err := tx.ExecContext(ctx, insertSetting, name); err != nil {
					return err
				}
				return call(ctx, tx)
			})
			if !errors.Is(err, store.ErrCrossClassWrite) {
				t.Fatalf("%s: class A writing events: err = %v, want ErrCrossClassWrite", name, err)
			}
		}
		if n := count(t, s.A(), "settings"); n != 0 {
			t.Errorf("settings holds %d rows after refused transactions, want 0 (rolled back)", n)
		}
		if n := count(t, s.B(), "events"); n != 0 {
			t.Errorf("events holds %d rows, want 0", n)
		}
		// The other directions too.
		for _, c := range []struct {
			h *store.Handle
			q string
		}{{s.B(), insertMessage}, {s.C(), insertSetting}, {s.C(), insertEvent}, {s.B(), insertSetting}} {
			err := c.h.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
				_, err := tx.ExecContext(ctx, c.q, "y")
				return err
			})
			if !errors.Is(err, store.ErrCrossClassWrite) {
				t.Errorf("class %s: %q: err = %v, want ErrCrossClassWrite", c.h.Class(), c.q, err)
			}
		}
	})
}

// TestNestedWriteRefused: no write transaction runs inside another's callback, whether the inner one is
// another class, the same class or a joint. With one write connection it would wait on its own caller's
// lock; it is refused at once instead.
func TestNestedWriteRefused(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		inner := map[string]func(ctx context.Context) error{
			"other class": func(ctx context.Context) error {
				return s.C().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error { return nil })
			},
			"same class": func(ctx context.Context) error {
				return s.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error { return nil })
			},
			"joint": func(ctx context.Context) error {
				return s.Joint(ctx, "RecordDeviceUse", func(ctx context.Context, tx store.Tx) error { return nil })
			},
		}
		for name, call := range inner {
			var innerErr error
			err := s.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
				if _, err := tx.ExecContext(ctx, insertSetting, name); err != nil {
					return err
				}
				innerErr = call(ctx)
				return innerErr
			})
			if !errors.Is(innerErr, store.ErrNestedWrite) || !errors.Is(err, store.ErrNestedWrite) {
				t.Fatalf("%s: inner err = %v, outer err = %v, want ErrNestedWrite", name, innerErr, err)
			}
		}
		if n := count(t, s.A(), "settings"); n != 0 {
			t.Errorf("settings holds %d rows, want 0 (each outer transaction rolled back)", n)
		}
	})
}

// TestJoint: an allowlisted joint writes exactly its classes, atomically; anything else is refused.
func TestJoint(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		if err := s.Joint(ctx, "BackfillEventIDs", func(ctx context.Context, tx store.Tx) error {
			if _, err := tx.ExecContext(ctx, insertSetting, "a"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, insertEvent, "b")
			return err
		}); err != nil {
			t.Fatalf("joint over A and B: %v", err)
		}
		if count(t, s.A(), "settings") != 1 || count(t, s.B(), "events") != 1 {
			t.Fatal("joint did not commit both writes")
		}
		err := s.Joint(ctx, "BackfillEventIDs", func(ctx context.Context, tx store.Tx) error {
			_, err := tx.ExecContext(ctx, insertMessage, "m")
			return err
		})
		if !errors.Is(err, store.ErrCrossClassWrite) {
			t.Errorf("joint over A and B writing C: err = %v, want ErrCrossClassWrite", err)
		}
		err = s.Joint(ctx, "SomethingNew", func(ctx context.Context, tx store.Tx) error { return nil })
		if !errors.Is(err, store.ErrUnknownJoint) {
			t.Errorf("unlisted joint: err = %v, want ErrUnknownJoint", err)
		}
	})
}

// TestReadsAcrossClasses: reads are not checked; a class's transaction may read any table (each
// cross-class read is a design input for the switch, not a refusal now).
func TestReadsAcrossClasses(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		if err := s.A().WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			var n int
			return tx.QueryRowContext(ctx, `SELECT count(*) FROM messages JOIN events ON 1 = 0`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestUnattributableRefused: a statement the Tx cannot attribute to a classed table is refused.
func TestUnattributableRefused(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		for _, q := range []string{
			`CREATE TABLE scratch (x INTEGER)`,
			`INSERT INTO goose_db_version (version_id, is_applied) VALUES (99, true)`,
			`INSERT INTO settings (scope, key, value_json) VALUES ('g', 'k', '1'); DELETE FROM events`,
		} {
			err := s.A().WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
				_, err := tx.ExecContext(ctx, q)
				return err
			})
			if !errors.Is(err, store.ErrUnclassified) {
				t.Errorf("%q: err = %v, want ErrUnclassified", q, err)
			}
		}
	})
}
