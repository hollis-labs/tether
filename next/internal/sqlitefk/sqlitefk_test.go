package sqlitefk_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/core/store/storetest"
	"modernc.org/sqlite"

	_ "github.com/hollis-labs/tether/next/internal/sqlitefk"
)

func init() {
	// A driver instance with no connection hooks: the control for what the hook changes.
	sql.Register("sqlite-without-hook", &sqlite.Driver{})
}

// TestControlWithoutHook: without the hook, the pragma go-store sends turns foreign keys on. The test
// below is only meaningful because this one holds.
func TestControlWithoutHook(t *testing.T) {
	db, err := sql.Open("sqlite-without-hook", "file:"+filepath.Join(t.TempDir(), "c.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var on int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatal(err)
	}
	if on != 1 {
		t.Fatalf("control: foreign_keys = %d without the hook, want 1", on)
	}
}

// TestForeignKeysOff: every connection of a go-store SQLite store (the writer and the read pool) reports
// foreign_keys off, although go-store asks for it on.
func TestForeignKeysOff(t *testing.T) {
	s := storetest.SQLite(t)
	ctx := context.Background()
	for name, q := range map[string]gostore.Querier{"reader": s.Reader()} {
		for i := 0; i < 3; i++ {
			var on int
			if err := q.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil {
				t.Fatal(err)
			}
			if on != 0 {
				t.Fatalf("%s connection: foreign_keys = %d, want 0", name, on)
			}
		}
	}
	if err := s.WriteTx(ctx, func(ctx context.Context, tx gostore.Tx) error {
		var on int
		if err := tx.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil {
			return err
		}
		if on != 0 {
			t.Errorf("writer connection: foreign_keys = %d, want 0", on)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestNoEnforcementNoCascade: an orphan row is accepted, and deleting a parent neither cascades nor fails.
func TestNoEnforcementNoCascade(t *testing.T) {
	s := storetest.SQLite(t)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE child (id TEXT PRIMARY KEY, parent_id TEXT REFERENCES parent(id) ON DELETE CASCADE)`,
		`INSERT INTO child VALUES ('orphan', 'missing')`,
		`INSERT INTO parent VALUES ('p')`,
		`INSERT INTO child VALUES ('c', 'p')`,
		`DELETE FROM parent WHERE id = 'p'`,
	} {
		if _, err := s.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n int
	if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM child`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("child holds %d rows, want 2 (the orphan kept, no cascade)", n)
	}
}
