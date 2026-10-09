package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// MessageRow is a non-destructive, all-recipients read of one row of the
// messages table, for the operator / Agent Ops dashboard.
//
// Unlike Inbox() — which atomically stamps delivered_at — reading via
// ListMessages never mutates lifecycle columns. Unlike the per-recipient
// List() (messaging_inbox.go), ListMessages is global: it scans every
// recipient so the dashboard can show all traffic. It carries the
// inbox-state columns (read_at, archived_at) and the subject/body payload
// projection. See Torque CW-20260517-0003.
type MessageRow struct {
	ID          string
	Kind        string
	Channel     string
	FromURN     string
	ToURN       string
	ThreadID    string
	InReplyTo   string
	Payload     string
	ContentType string
	CreatedAt   string
	DeliveredAt string
	ConsumedAt  string
	CanceledAt  string
	ReadAt      string
	ArchivedAt  string
	// Subject and Body are the human-readable projection of Payload —
	// see projectPayload (messaging_inbox.go) for the contract.
	Subject string
	Body    string
}

// MessageQuery selects a non-destructive operator inbox page.
type MessageQuery struct {
	Scope     string
	Recipient string
	Read      string // all, read, unread
	Archive   string // all, active, archived
	Limit     int
	Offset    int
}

type MessagePage struct {
	Messages []MessageRow
	Total    int
	Limit    int
	Offset   int
}

func (s *Store) ListMessages(limit int) ([]MessageRow, error) {
	page, err := s.ListMessagesPage(MessageQuery{Limit: limit})
	return page.Messages, err
}

// ListMessagesPage applies mailbox filters before a bounded page. Count and
// rows share a read snapshot; listing never changes message lifecycle state.
func (s *Store) ListMessagesPage(q MessageQuery) (MessagePage, error) {
	where := "routing_staged=0"
	args := []any{}
	switch q.Scope {
	case "", "all":
	case "user", "agent":
		where += " AND to_urn LIKE ?"
		args = append(args, "msg://"+q.Scope+"/%")
	default:
		return MessagePage{}, fmt.Errorf("invalid message scope %q", q.Scope)
	}
	if q.Recipient != "" {
		where += " AND to_urn=?"
		args = append(args, q.Recipient)
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
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	page := MessagePage{Limit: q.Limit, Offset: q.Offset}
	tx, err := s.db.Begin()
	if err != nil {
		return page, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRow("SELECT COUNT(*) FROM messages WHERE "+where, args...).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := tx.Query(
		`SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
          payload, content_type, created_at, delivered_at, consumed_at,
          canceled_at, read_at, archived_at
   FROM messages WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, q.Limit, q.Offset)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		var channel, threadID, inReplyTo, payload, contentType sql.NullString
		var deliveredAt, consumedAt, canceledAt, readAt, archivedAt sql.NullString
		if err := rows.Scan(
			&m.ID, &m.Kind, &channel, &m.FromURN, &m.ToURN,
			&threadID, &inReplyTo, &payload, &contentType, &m.CreatedAt,
			&deliveredAt, &consumedAt, &canceledAt, &readAt, &archivedAt,
		); err != nil {
			return page, err
		}
		m.Channel = channel.String
		m.ThreadID = threadID.String
		m.InReplyTo = inReplyTo.String
		m.Payload = payload.String
		m.ContentType = contentType.String
		m.DeliveredAt = deliveredAt.String
		m.ConsumedAt = consumedAt.String
		m.CanceledAt = canceledAt.String
		m.ReadAt = readAt.String
		m.ArchivedAt = archivedAt.String
		m.Subject, m.Body = projectPayload(json.RawMessage(m.Payload))
		out = append(out, m)
	}
	page.Messages = out
	return page, rows.Err()
}
