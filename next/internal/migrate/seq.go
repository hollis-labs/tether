package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// VerifyMessagesSeq is the acceptance check for messages.seq (port plan section 13.1): after migrate-from
// (S12) copies messages, every message in the new database carries seq equal to the rowid it had in the
// old one, so the mailbox watch cursors clients hold keep their meaning. It walks both tables in cursor
// order and fails on the first message whose id or position differs, or on a count mismatch.
//
// old is the source snapshot the copy read (never a database that was VACUUMed after the copy: VACUUM may
// renumber the rowids of a table without an INTEGER PRIMARY KEY). nw is the new database, on either
// dialect; the queries take no parameters, so they need no rebinding.
func VerifyMessagesSeq(ctx context.Context, old, nw *sql.DB) error {
	o, err := old.QueryContext(ctx, `SELECT id, rowid FROM messages ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("messages seq: read old: %w", err)
	}
	defer func() { _ = o.Close() }()
	n, err := nw.QueryContext(ctx, `SELECT id, seq FROM messages ORDER BY seq`)
	if err != nil {
		return fmt.Errorf("messages seq: read new: %w", err)
	}
	defer func() { _ = n.Close() }()
	var count int64
	for {
		oNext, nNext := o.Next(), n.Next()
		if oNext && !nNext {
			return fmt.Errorf("messages seq: the new database ends after %d messages; the old one has more", count)
		}
		if nNext && !oNext {
			return fmt.Errorf("messages seq: the old database ends after %d messages; the new one has more", count)
		}
		if !oNext {
			break
		}
		var oid, nid string
		var rowid, seq int64
		if err := o.Scan(&oid, &rowid); err != nil {
			return fmt.Errorf("messages seq: scan old: %w", err)
		}
		if err := n.Scan(&nid, &seq); err != nil {
			return fmt.Errorf("messages seq: scan new: %w", err)
		}
		if oid != nid || rowid != seq {
			return fmt.Errorf("messages seq: position %d: old %s at rowid %d, new %s at seq %d", count+1, oid, rowid, nid, seq)
		}
		count++
	}
	if err := o.Err(); err != nil {
		return fmt.Errorf("messages seq: read old: %w", err)
	}
	if err := n.Err(); err != nil {
		return fmt.Errorf("messages seq: read new: %w", err)
	}
	return nil
}
