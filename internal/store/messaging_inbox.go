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
	Limit           int
	IncludeArchived bool // when true, archived messages are included
	UnreadOnly      bool // when true, only messages with read_at IS NULL
}

// InboxStore extends messaging.Store with non-destructive inbox semantics:
// repeatable listing, explicit read state, and soft-delete archive. The
// embedded messaging.Store keeps the atomic-delivery pull model (Inbox)
// intact for agent consumers (ADR-0023 §4); the methods added here never
// mutate delivered_at.
type InboxStore interface {
	messaging.Store

	// List returns the recipient's messages with no lifecycle side effect.
	// It may be called repeatedly; delivered_at/read_at are never touched.
	// Archived messages are excluded unless ListFilter.IncludeArchived.
	List(ctx context.Context, to messaging.Address, f ListFilter) ([]Message, error)

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
func (ms *messagingStore) List(ctx context.Context, to messaging.Address, f ListFilter) ([]Message, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
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

	rows, err := ms.db.QueryContext(ctx,
		`SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
		        payload, content_type, metadata, created_at,
		        delivered_at, consumed_at, read_at, archived_at
		 FROM messages WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("messaging store: list query: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
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

// stampRecipientField sets (clear=false) or clears (clear=true) a
// timestamp column scoped to the (id, recipient) pair. It mirrors the
// Consume idempotency pattern: a no-op UPDATE is disambiguated into
// not-found / wrong-recipient / already-in-target-state.
//
// col is an internal constant ("read_at"/"archived_at"), never caller
// input — it is safe to interpolate into the statement.
func (ms *messagingStore) stampRecipientField(ctx context.Context, id string, recipient messaging.Address, col string, clear bool) error {
	var (
		res sql.Result
		err error
	)
	if clear {
		res, err = ms.db.ExecContext(ctx,
			`UPDATE messages SET `+col+`=NULL WHERE id=? AND to_urn=? AND `+col+` IS NOT NULL`,
			id, recipient.URN())
	} else {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		res, err = ms.db.ExecContext(ctx,
			`UPDATE messages SET `+col+`=? WHERE id=? AND to_urn=? AND `+col+` IS NULL`,
			now, id, recipient.URN())
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

// scanMessage scans a 15-column messages row (the 13 envelope columns plus
// read_at, archived_at) into a Message and derives its subject/body.
func scanMessage(scan scanFunc) (Message, error) {
	var (
		m                                                     Message
		kindStr, channelStr, fromURN, toURN                   string
		threadID, inReplyTo, payloadStr, contentType, metaStr sql.NullString
		createdStr                                            string
		deliveredStr, consumedStr, readStr, archivedStr       sql.NullString
	)
	err := scan(
		&m.ID, &kindStr, &channelStr, &fromURN, &toURN,
		&threadID, &inReplyTo, &payloadStr, &contentType, &metaStr,
		&createdStr, &deliveredStr, &consumedStr, &readStr, &archivedStr,
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
