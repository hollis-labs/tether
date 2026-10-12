package store

// messaging_watch.go -- cursor reads behind the mailbox watch stream
// (CW-20261012-0021). Like List, nothing here touches delivered_at or
// read_at: watching a mailbox is not receiving from it.

import (
	"context"
	"fmt"

	"github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

// MailboxEntry is a message with its mailbox cursor. Seq is the messages
// table rowid: monotonic per insert, stable for a row's lifetime, and shared
// across mailboxes, so a recipient sees increasing but non-contiguous values.
type MailboxEntry struct {
	Seq int64
	Message
}

// MailboxWindow is one bounded read after a cursor. HighWater is the table's
// newest rowid at read time, whatever the filter matched.
type MailboxWindow struct {
	Entries   []MailboxEntry
	HighWater int64
}

// MailboxWatcher is the optional store capability behind GET /messages/watch.
type MailboxWatcher interface {
	MailboxAfter(ctx context.Context, to messaging.Address, after int64, f ListFilter, limit int) (MailboxWindow, error)
}

var _ MailboxWatcher = (*messagingStore)(nil)

// MailboxAfter returns up to limit of the recipient's messages with a cursor
// strictly greater than after, oldest first, using List's filter semantics.
func (ms *messagingStore) MailboxAfter(ctx context.Context, to messaging.Address, after int64, f ListFilter, limit int) (MailboxWindow, error) {
	if _, ok := channels.AddressName(to); ok {
		return MailboxWindow{}, channels.ErrMailboxOperation
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var win MailboxWindow
	if err := ms.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) FROM messages`).Scan(&win.HighWater); err != nil {
		return MailboxWindow{}, fmt.Errorf("messaging store: mailbox high water: %w", err)
	}
	where, args := listWhere(to, f)
	//nolint:gosec // G202: where comes from listWhere (constant fragments plus generated ? placeholders); every value is a bound argument
	rows, err := ms.db.QueryContext(ctx,
		`SELECT rowid, id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
		        payload, content_type, metadata, created_at,
		        delivered_at, consumed_at, read_at, archived_at, canceled_at
		 FROM messages WHERE rowid > ? AND rowid <= ? AND `+where+` ORDER BY rowid LIMIT ?`,
		append([]any{after, win.HighWater}, append(args, limit)...)...)
	if err != nil {
		return MailboxWindow{}, fmt.Errorf("messaging store: mailbox query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e MailboxEntry
		e.Message, err = scanMessage(func(dest ...any) error {
			return rows.Scan(append([]any{&e.Seq}, dest...)...)
		})
		if err != nil {
			return MailboxWindow{}, err
		}
		win.Entries = append(win.Entries, e)
	}
	return win, rows.Err()
}
