package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/core/store"
)

// oldMessages builds an old database holding messages whose rowids have gaps, as a live mailbox does
// after purges: m1..m6 inserted, m2 and m5 deleted.
func oldMessages(t *testing.T) *sql.DB {
	t.Helper()
	old := buildOld(t)
	for i := 1; i <= 6; i++ {
		if _, err := old.Exec(`INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES (?, 'notice', 'f', 't', 'c')`, fmt.Sprintf("m%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(`DELETE FROM messages WHERE id IN ('m2', 'm5')`); err != nil {
		t.Fatal(err)
	}
	return old
}

// copyMessages copies the old messages into the new database the way migrate-from (S12) must: with seq
// set explicitly to the old rowid. With explicitSeq false it lets the new database assign seq instead,
// which is the mistake VerifyMessagesSeq exists to catch.
func copyMessages(t *testing.T, old *sql.DB, nw *store.Store, explicitSeq bool) {
	t.Helper()
	ctx := context.Background()
	rows, err := old.Query(`SELECT rowid, id FROM messages ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var rowid int64
		var id string
		if err := rows.Scan(&rowid, &id); err != nil {
			t.Fatal(err)
		}
		q := `INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES (?, 'notice', 'f', 't', 'c')`
		args := []any{id}
		if explicitSeq {
			q = `INSERT INTO messages (id, kind, from_urn, to_urn, created_at, seq) VALUES (?, 'notice', 'f', 't', 'c', ?)`
			args = append(args, rowid)
		}
		if _, err := nw.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if nw.Dialect().Name() == store.DriverPostgres {
		// Explicit values do not move an identity column: migrate-from must set it past the highest seq
		// it wrote, or the next new message collides with a migrated one.
		if _, err := nw.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('messages', 'seq'), (SELECT max(seq) FROM messages))`); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMessagesSeqAcceptance states the acceptance for messages.seq: after migrate-from, seq equals the
// old rowid for every message (gaps included), VerifyMessagesSeq proves it, and a new message continues
// past the highest migrated seq, so a watcher's cursor never moves backwards across the cutover.
func TestMessagesSeqAcceptance(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *store.Store) {
		ctx := context.Background()
		old := oldMessages(t)
		copyMessages(t, old, s, true)
		if err := VerifyMessagesSeq(ctx, old, s.DB()); err != nil {
			t.Fatalf("verify after a correct copy: %v", err)
		}
		if _, err := s.ExecContext(ctx, `INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES ('new', 'notice', 'f', 't', 'c')`); err != nil {
			t.Fatal(err)
		}
		var seq int64
		if err := s.DB().QueryRowContext(ctx, `SELECT seq FROM messages WHERE id = 'new'`).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if seq != 7 {
			t.Errorf("a new message after the copy got seq %d, want 7 (past the old high-water rowid 6)", seq)
		}
	})
}

// TestMessagesSeqVerifyCatchesRenumbering proves the check fails when seq is not the old rowid: a copy
// that lets the database number the rows closes the gaps and moves every cursor after the first gap.
func TestMessagesSeqVerifyCatchesRenumbering(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *store.Store) {
		old := oldMessages(t)
		copyMessages(t, old, s, false)
		err := VerifyMessagesSeq(context.Background(), old, s.DB())
		if err == nil || !strings.Contains(err.Error(), "old m3 at rowid 3, new m3 at seq 2") {
			t.Fatalf("verify after a renumbering copy = %v, want the m3 mismatch", err)
		}
	})
}

// TestMessagesSeqVerifyCatchesMissingRows proves the check fails when the copy dropped a message.
func TestMessagesSeqVerifyCatchesMissingRows(t *testing.T) {
	ctx := context.Background()
	s := sqliteStore(t)
	old := oldMessages(t)
	copyMessages(t, old, s, true)
	if _, err := s.ExecContext(ctx, `DELETE FROM messages WHERE id = 'm6'`); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMessagesSeq(ctx, old, s.DB()); err == nil || !strings.Contains(err.Error(), "the new database ends after 3 messages") {
		t.Fatalf("verify after a short copy = %v, want the count mismatch", err)
	}
}
