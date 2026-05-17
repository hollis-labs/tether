package store

// messaging_inbox.go — non-destructive inbox semantics for the messages
// table (CW-20260517-0003). The methods here never mutate delivered_at;
// they implement repeatable listing, explicit read state, and soft-delete
// archive. The atomic-delivery pull model (Inbox, in messaging_store.go)
// stays intact for agent consumers — see ADR-0023 §4.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/go-messaging"
)

// Message is a messaging envelope enriched with the inbox-state columns
// added in migration 0013 (read_at, archived_at) plus a human-readable
// subject/body projection of the payload. It is the DTO returned by the
// non-destructive List path; the agent pull model (Inbox) still returns
// bare messaging.Envelope values.
type Message struct {
	messaging.Envelope
	ReadAt     *time.Time `json:"read_at"`
	ArchivedAt *time.Time `json:"archived_at"`
	CanceledAt *time.Time `json:"canceled_at"`
	// Subject and Body are a display projection of Payload — see
	// projectPayload for the contract. Empty when Payload yields nothing.
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
}

// ListFilter narrows the non-destructive List query. The zero value lists
// every non-archived message for the recipient, newest first.
type ListFilter struct {
	Kind            []messaging.Kind
	ThreadID        string
	Limit           int  // clamped to [1,100] by List; 0 → default 100
	Offset          int  // floored at 0 by List
	IncludeArchived bool // when true, archived messages are included
	UnreadOnly      bool // when true, only messages with read_at IS NULL
}

// ListPage is the paginated result of List: the page of messages plus the
// total count of rows matching the filter (before LIMIT/OFFSET) and the
// effective Limit/Offset that were applied.
type ListPage struct {
	Messages []Message `json:"messages"`
	Total    int       `json:"total"`
	Limit    int       `json:"limit"`
	Offset   int       `json:"offset"`
}

// InboxStore extends messaging.Store with non-destructive inbox semantics:
// repeatable listing, explicit read state, and soft-delete archive. The
// embedded messaging.Store keeps the atomic-delivery pull model (Inbox)
// intact for agent consumers (ADR-0023 §4); the methods added here never
// mutate delivered_at.
type InboxStore interface {
	messaging.Store

	// List returns a page of the recipient's messages with no lifecycle
	// side effect. It may be called repeatedly; delivered_at/read_at are
	// never touched. Archived messages are excluded unless
	// ListFilter.IncludeArchived. The returned ListPage carries the total
	// count of matching rows for pagination.
	List(ctx context.Context, to messaging.Address, f ListFilter) (ListPage, error)

	// MarkRead stamps read_at for (id, recipient). Idempotent: a second
	// call is a no-op. Returns messaging.ErrNotFound if the id is absent
	// and ErrWrongRecipient if recipient is not the envelope's to_urn.
	MarkRead(ctx context.Context, id string, recipient messaging.Address) error

	// Archive soft-deletes a message by stamping archived_at for
	// (id, recipient). Archived messages drop out of default List results.
	// Idempotent; same error semantics as MarkRead.
	Archive(ctx context.Context, id string, recipient messaging.Address) error

	// Unarchive clears archived_at, restoring the message to default List
	// results. Idempotent; same error semantics as MarkRead.
	Unarchive(ctx context.Context, id string, recipient messaging.Address) error
}

// compile-time assertion: the SQLite store satisfies the extended contract.
var _ InboxStore = (*messagingStore)(nil)

// ─── List ──────────────────────────────────────────────────────────────────

// List returns a recipient's messages without any lifecycle side effect.
// Unlike Inbox it never stamps delivered_at, so a UI can poll it
// repeatedly. Results are ordered newest-first (created_at DESC, id DESC).
//
// Limit is clamped to [1,100] (0 → default 100, hard max 100) and Offset
// is floored at 0. The returned ListPage.Total is a COUNT(*) over the same
// WHERE clause, taken before LIMIT/OFFSET, so callers can paginate.
func (ms *messagingStore) List(ctx context.Context, to messaging.Address, f ListFilter) (ListPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	where := "to_urn=?"
	args := []any{to.URN()}
	if !f.IncludeArchived {
		where += " AND archived_at IS NULL"
	}
	if f.UnreadOnly {
		where += " AND read_at IS NULL"
	}
	if len(f.Kind) > 0 {
		in := make([]any, len(f.Kind))
		for i, k := range f.Kind {
			in[i] = string(k)
		}
		where += " AND kind IN (?" + repeatCommaQ(len(f.Kind)-1) + ")"
		args = append(args, in...)
	}
	if f.ThreadID != "" {
		where += " AND thread_id=?"
		args = append(args, f.ThreadID)
	}

	page := ListPage{Limit: limit, Offset: offset}

	// Total count over the same WHERE clause, before LIMIT/OFFSET.
	if err := ms.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE `+where, args...,
	).Scan(&page.Total); err != nil {
		return ListPage{}, fmt.Errorf("messaging store: list count: %w", err)
	}

	rows, err := ms.db.QueryContext(ctx,
		`SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
		        payload, content_type, metadata, created_at,
		        delivered_at, consumed_at, read_at, archived_at, canceled_at
		 FROM messages WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return ListPage{}, fmt.Errorf("messaging store: list query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		m, err := scanMessage(rows.Scan)
		if err != nil {
			return ListPage{}, err
		}
		page.Messages = append(page.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return ListPage{}, err
	}
	return page, nil
}

// ─── MarkRead / Archive / Unarchive ─────────────────────────────────────────

// MarkRead stamps read_at for (id, recipient). Idempotent.
func (ms *messagingStore) MarkRead(ctx context.Context, id string, recipient messaging.Address) error {
	return ms.stampRecipientField(ctx, id, recipient, "read_at", false)
}

// Archive stamps archived_at for (id, recipient). Idempotent.
func (ms *messagingStore) Archive(ctx context.Context, id string, recipient messaging.Address) error {
	return ms.stampRecipientField(ctx, id, recipient, "archived_at", false)
}

// Unarchive clears archived_at for (id, recipient). Idempotent.
func (ms *messagingStore) Unarchive(ctx context.Context, id string, recipient messaging.Address) error {
	return ms.stampRecipientField(ctx, id, recipient, "archived_at", true)
}

// stampRecipientField sets (clr=false) or clears (clr=true) a timestamp
// column scoped to the (id, recipient) pair. It mirrors the Consume
// idempotency pattern: a no-op UPDATE is disambiguated into not-found /
// wrong-recipient / already-in-target-state.
//
// col selects the column ("read_at"/"archived_at"); each branch uses a
// fully static statement so no SQL is ever assembled from a variable.
func (ms *messagingStore) stampRecipientField(ctx context.Context, id string, recipient messaging.Address, col string, clr bool) error {
	var (
		res sql.Result
		err error
	)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	switch {
	case col == "read_at" && !clr:
		res, err = ms.db.ExecContext(ctx,
			`UPDATE messages SET read_at=? WHERE id=? AND to_urn=? AND read_at IS NULL`,
			now, id, recipient.URN())
	case col == "read_at" && clr:
		res, err = ms.db.ExecContext(ctx,
			`UPDATE messages SET read_at=NULL WHERE id=? AND to_urn=? AND read_at IS NOT NULL`,
			id, recipient.URN())
	case col == "archived_at" && !clr:
		res, err = ms.db.ExecContext(ctx,
			`UPDATE messages SET archived_at=? WHERE id=? AND to_urn=? AND archived_at IS NULL`,
			now, id, recipient.URN())
	case col == "archived_at" && clr:
		res, err = ms.db.ExecContext(ctx,
			`UPDATE messages SET archived_at=NULL WHERE id=? AND to_urn=? AND archived_at IS NOT NULL`,
			id, recipient.URN())
	default:
		return fmt.Errorf("messaging store: stampRecipientField: unknown column %q", col)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil // updated — success
	}
	// 0 rows: not found, wrong recipient, or already in the target state.
	var toURN string
	err = ms.db.QueryRowContext(ctx, `SELECT to_urn FROM messages WHERE id=?`, id).Scan(&toURN)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return messaging.ErrNotFound
		}
		return err
	}
	if toURN != recipient.URN() {
		return ErrWrongRecipient
	}
	return nil // row exists, recipient matches — already in target state. Idempotent.
}

// ─── Scan + payload projection ──────────────────────────────────────────────

// scanMessage scans a 16-column messages row (the 13 envelope columns plus
// read_at, archived_at, canceled_at) into a Message and derives its
// subject/body.
func scanMessage(scan scanFunc) (Message, error) {
	var (
		m                                                     Message
		kindStr, channelStr, fromURN, toURN                   string
		threadID, inReplyTo, payloadStr, contentType, metaStr sql.NullString
		createdStr                                            string
		deliveredStr, consumedStr, readStr, archivedStr       sql.NullString
		canceledStr                                           sql.NullString
	)
	err := scan(
		&m.ID, &kindStr, &channelStr, &fromURN, &toURN,
		&threadID, &inReplyTo, &payloadStr, &contentType, &metaStr,
		&createdStr, &deliveredStr, &consumedStr, &readStr, &archivedStr,
		&canceledStr,
	)
	if err != nil {
		return Message{}, err
	}

	m.Kind = messaging.Kind(kindStr)
	m.Channel = messaging.Channel(channelStr)
	m.ThreadID = threadID.String
	m.InReplyTo = inReplyTo.String
	m.ContentType = contentType.String

	from, err := messaging.ParseURN(fromURN)
	if err != nil {
		return Message{}, fmt.Errorf("parse from_urn %q: %w", fromURN, err)
	}
	m.From = from
	to, err := messaging.ParseURN(toURN)
	if err != nil {
		return Message{}, fmt.Errorf("parse to_urn %q: %w", toURN, err)
	}
	m.To = to

	if payloadStr.Valid && payloadStr.String != "" {
		m.Payload = json.RawMessage(payloadStr.String)
	}
	if metaStr.Valid && metaStr.String != "" {
		_ = json.Unmarshal([]byte(metaStr.String), &m.Metadata)
	}

	m.CreatedAt = parseMsgTime(createdStr)
	m.DeliveredAt = parseMsgNullTime(deliveredStr)
	m.ConsumedAt = parseMsgNullTime(consumedStr)
	m.ReadAt = parseMsgNullTime(readStr)
	m.ArchivedAt = parseMsgNullTime(archivedStr)
	m.CanceledAt = parseMsgNullTime(canceledStr)

	m.Subject, m.Body = projectPayload(m.Payload)
	return m, nil
}

// parseMsgTime parses an RFC3339(Nano) timestamp, tolerating either form.
func parseMsgTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t
}

// parseMsgNullTime parses a nullable RFC3339(Nano) column into *time.Time,
// returning nil for NULL/empty/unparseable values.
func parseMsgNullTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseMsgTime(ns.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

// projectPayload derives a human-readable subject/body from a message
// payload for email-style display surfaces.
//
// Payload display contract:
//
//   - Structured object — a JSON object payload projects Subject from the
//     "subject" or "title" key and Body from the first non-empty of
//     "body", "summary", "text", "message".
//   - JSON string — a JSON-encoded string payload projects Body = the
//     decoded string, Subject = "".
//   - Other — any other payload (raw text, number, array) projects
//     Body = the raw payload text, Subject = "".
//
// Producers that want a clean email-style render SHOULD send a JSON
// object shaped {"subject": "...", "body": "..."}. The raw Payload is
// always preserved on the DTO for callers that need the full structure.
func projectPayload(payload json.RawMessage) (subject, body string) {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return "", ""
	}
	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			return "", trimmed
		}
		return firstString(obj, "subject", "title"),
			firstString(obj, "body", "summary", "text", "message")
	case '"':
		var s string
		if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
			return "", s
		}
		return "", trimmed
	default:
		return "", trimmed
	}
}

// firstString returns the first key in obj whose value is a non-empty
// JSON string. Used by projectPayload to resolve subject/body aliases.
func firstString(obj map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
