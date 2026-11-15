package msgstore

import (
	"context"
	"database/sql"
	"fmt"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
)

// ListFilter narrows List and MailboxAfter.
type ListFilter struct {
	Kind            []messaging.Kind
	ThreadID        string
	Limit           int  // clamped to [1,100]; 0 means 100
	Offset          int  // floored at 0
	IncludeArchived bool // include archived messages
	UnreadOnly      bool // only read_at IS NULL
}

// ListPage is one page of a recipient's messages.
type ListPage struct {
	Messages []Message `json:"messages"`
	Total    int       `json:"total"`
	Limit    int       `json:"limit"`
	Offset   int       `json:"offset"`
}

func listWhere(to messaging.Address, f ListFilter) (string, []any) {
	where := "routing_staged = 0 AND to_urn = ?"
	args := []any{to.URN()}
	if !f.IncludeArchived {
		where += " AND archived_at IS NULL"
	}
	if f.UnreadOnly {
		where += " AND read_at IS NULL"
	}
	kinds, kargs := kindIn(f.Kind)
	where += kinds
	args = append(args, kargs...)
	if f.ThreadID != "" {
		where, args = where+" AND thread_id = ?", append(args, f.ThreadID)
	}
	return where, args
}

// List returns a recipient's messages newest first with the total, read in one snapshot.
func (s *Store) List(ctx context.Context, to messaging.Address, f ListFilter) (ListPage, error) {
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	offset := max(f.Offset, 0)
	where, args := listWhere(to, f)
	page := ListPage{Limit: limit, Offset: offset}
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		if err := q.QueryRowContext(ctx, rebind(q, `SELECT COUNT(*) FROM messages WHERE `+where), args...).Scan(&page.Total); err != nil {
			return fmt.Errorf("messaging store: list count: %w", err)
		}
		rows, err := q.QueryContext(ctx, rebind(q, `SELECT `+messageColumns+` FROM messages WHERE `+where+
			` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`), append(args, limit, offset)...)
		if err != nil {
			return fmt.Errorf("messaging store: list query: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			m, err := scanMessage(rows.Scan)
			if err != nil {
				return err
			}
			page.Messages = append(page.Messages, m)
		}
		return rows.Err()
	})
	if err != nil {
		return ListPage{}, err
	}
	return page, nil
}

// MarkRead marks a message read for its recipient. MarkRead, Archive and Unarchive are idempotent
// for the recipient; a missing message is messaging.ErrNotFound and another recipient's is
// ErrWrongRecipient.
func (s *Store) MarkRead(ctx context.Context, id string, recipient messaging.Address) error {
	return s.stampRecipient(ctx, id, recipient, `UPDATE messages SET read_at = ? WHERE routing_staged = 0 AND id = ? AND to_urn = ? AND read_at IS NULL`, true)
}

// Archive marks a message archived for its recipient.
func (s *Store) Archive(ctx context.Context, id string, recipient messaging.Address) error {
	return s.stampRecipient(ctx, id, recipient, `UPDATE messages SET archived_at = ? WHERE routing_staged = 0 AND id = ? AND to_urn = ? AND archived_at IS NULL`, true)
}

// Unarchive clears archived_at for the recipient.
func (s *Store) Unarchive(ctx context.Context, id string, recipient messaging.Address) error {
	return s.stampRecipient(ctx, id, recipient, `UPDATE messages SET archived_at = NULL WHERE routing_staged = 0 AND id = ? AND to_urn = ? AND archived_at IS NOT NULL`, false)
}

func (s *Store) stampRecipient(ctx context.Context, id string, recipient messaging.Address, update string, withTime bool) error {
	if err := s.RequireMailbox(ctx, id); err != nil {
		return err
	}
	args := []any{id, recipient.URN()}
	if withTime {
		args = append([]any{stamp(s.now())}, args...)
	}
	var n int64
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, update), args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil || n > 0 {
		return err
	}
	return s.recipientOf(ctx, id, recipient)
}

// MessageRow is the operator listing's flat row (times as stored, RFC 3339 text).
type MessageRow struct {
	ID, Kind, Channel, FromURN, ToURN, ThreadID, InReplyTo, Payload, ContentType string
	CreatedAt, DeliveredAt, ConsumedAt, CanceledAt, ReadAt, ArchivedAt           string
	Subject, Body                                                                string
}

// MessageQuery filters ListMessagesPage. Scope is all|user|agent (recipient URN kind), Read
// all|read|unread, Archive all|active|archived.
type MessageQuery struct {
	Scope, Recipient, Read, Archive string
	Limit, Offset                   int
}

// MessagePage is one operator page.
type MessagePage struct {
	Messages []MessageRow
	Total    int
	Limit    int
	Offset   int
}

// ListMessagesPage is the operator's view across mailboxes, newest first (limit 1..1000, default
// 200), counted and read in one snapshot.
func (s *Store) ListMessagesPage(ctx context.Context, q MessageQuery) (MessagePage, error) {
	where := "routing_staged = 0"
	var args []any
	switch q.Scope {
	case "", "all":
	case "user", "agent":
		where, args = where+" AND to_urn LIKE ?", append(args, "msg://"+q.Scope+"/%")
	default:
		return MessagePage{}, fmt.Errorf("invalid message scope %q", q.Scope)
	}
	if q.Recipient != "" {
		where, args = where+" AND to_urn = ?", append(args, q.Recipient)
	}
	switch q.Read {
	case "", "all":
	case "read":
		where += " AND read_at IS NOT NULL"
	case "unread":
		where += " AND read_at IS NULL AND canceled_at IS NULL"
	default:
		return MessagePage{}, fmt.Errorf("invalid read filter %q", q.Read)
	}
	switch q.Archive {
	case "", "all":
	case "active":
		where += " AND archived_at IS NULL"
	case "archived":
		where += " AND archived_at IS NOT NULL"
	default:
		return MessagePage{}, fmt.Errorf("invalid archive filter %q", q.Archive)
	}
	if q.Limit <= 0 {
		q.Limit = 200
	}
	q.Limit, q.Offset = min(q.Limit, 1000), max(q.Offset, 0)
	page := MessagePage{Limit: q.Limit, Offset: q.Offset}
	err := s.c.ReadTx(ctx, func(ctx context.Context, rq gostore.Querier) error {
		if err := rq.QueryRowContext(ctx, rebind(rq, `SELECT COUNT(*) FROM messages WHERE `+where), args...).Scan(&page.Total); err != nil {
			return err
		}
		rows, err := rq.QueryContext(ctx, rebind(rq, `SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to, payload, content_type,
 created_at, delivered_at, consumed_at, canceled_at, read_at, archived_at
 FROM messages WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`), append(args, q.Limit, q.Offset)...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var m MessageRow
			var channel, thread, reply, payload, ct, delivered, consumed, canceled, read, archived sql.NullString
			if err := rows.Scan(&m.ID, &m.Kind, &channel, &m.FromURN, &m.ToURN, &thread, &reply, &payload, &ct, &m.CreatedAt,
				&delivered, &consumed, &canceled, &read, &archived); err != nil {
				return err
			}
			m.Channel, m.ThreadID, m.InReplyTo, m.Payload, m.ContentType = channel.String, thread.String, reply.String, payload.String, ct.String
			m.DeliveredAt, m.ConsumedAt, m.CanceledAt, m.ReadAt, m.ArchivedAt = delivered.String, consumed.String, canceled.String, read.String, archived.String
			m.Subject, m.Body = ProjectPayload([]byte(m.Payload))
			page.Messages = append(page.Messages, m)
		}
		return rows.Err()
	})
	if err != nil {
		return MessagePage{}, err
	}
	return page, nil
}

// MailboxEntry is a message with its watch cursor.
type MailboxEntry struct {
	Seq int64
	Message
}

// MailboxWindow is one watch step: the entries after the cursor up to the high-water mark.
type MailboxWindow struct {
	Entries   []MailboxEntry
	HighWater int64
}

// MailboxAfter returns the recipient's messages with seq in (after, high water], oldest first
// (limit 1..100). seq holds the old rowid values (S1b), so cursors clients hold stay valid; the
// high-water mark and the window are read in one snapshot, and LockMessageSeq keeps Postgres from
// committing seq values out of order.
func (s *Store) MailboxAfter(ctx context.Context, to messaging.Address, after int64, f ListFilter, limit int) (MailboxWindow, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var win MailboxWindow
	where, args := listWhere(to, f)
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM messages`).Scan(&win.HighWater); err != nil {
			return fmt.Errorf("messaging store: mailbox high water: %w", err)
		}
		rows, err := q.QueryContext(ctx, rebind(q, `SELECT `+messageColumns+`, seq FROM messages WHERE seq > ? AND seq <= ? AND `+where+` ORDER BY seq LIMIT ?`),
			append([]any{after, win.HighWater}, append(args, limit)...)...)
		if err != nil {
			return fmt.Errorf("messaging store: mailbox query: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var e MailboxEntry
			if e.Message, err = scanMessage(rows.Scan, &e.Seq); err != nil {
				return err
			}
			win.Entries = append(win.Entries, e)
		}
		return rows.Err()
	})
	if err != nil {
		return MailboxWindow{}, err
	}
	return win, nil
}
