package store

import "database/sql"

// MessageRow is a non-destructive read of one row of the messages table,
// for operator / dashboard views.
//
// Unlike Inbox() — which atomically stamps delivered_at on every row it
// returns — reading via ListMessages never mutates lifecycle columns. It
// is a pure SELECT, safe to poll repeatedly. See Torque CW-20260517-0003
// for the planned first-class inbox semantics this stopgap anticipates.
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
}

// ListMessages returns the most recent messages across all recipients,
// newest first, up to limit (defaulted to 200, capped at 1000). It is a
// pure read and never marks messages delivered.
func (s *Store) ListMessages(limit int) ([]MessageRow, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.Query(
		`SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
		        payload, content_type, created_at, delivered_at, consumed_at, canceled_at
		 FROM messages ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		var channel, threadID, inReplyTo, payload, contentType sql.NullString
		var deliveredAt, consumedAt, canceledAt sql.NullString
		if err := rows.Scan(
			&m.ID, &m.Kind, &channel, &m.FromURN, &m.ToURN,
			&threadID, &inReplyTo, &payload, &contentType, &m.CreatedAt,
			&deliveredAt, &consumedAt, &canceledAt,
		); err != nil {
			return nil, err
		}
		m.Channel = channel.String
		m.ThreadID = threadID.String
		m.InReplyTo = inReplyTo.String
		m.Payload = payload.String
		m.ContentType = contentType.String
		m.DeliveredAt = deliveredAt.String
		m.ConsumedAt = consumedAt.String
		m.CanceledAt = canceledAt.String
		out = append(out, m)
	}
	return out, rows.Err()
}
