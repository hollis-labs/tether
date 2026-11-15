// Package msgstore is the class C store for Tether's own messaging tables (port slice S2b-2a, plan
// 13.1): messages (the mailbox), aliases, pending replies, broker envelopes and message retention. It
// reaches its tables only through the class C handle and writes SQL that runs on SQLite and
// Postgres. go-messaging's messaging_* tables stay the library's.
//
// The store speaks go-messaging envelopes (a library type) and plain rows. What the old store
// layered on top (internal/store/messaging_*.go at main f6c37bfa) is the messaging slice's: the
// library Store/Inbox interfaces with subscriber fan-out and the delivery-backed wrapper (S6), and the
// channel publication rules and mailbox-address checks (S7). The SQL guards that read only messaging
// tables stay here.
package msgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
)

var (
	// ErrWrongRecipient: the caller is not the message's recipient.
	ErrWrongRecipient = errors.New("consume: caller is not the intended recipient")
	// ErrPublicationNotMailbox: the id is a channel publication, which has no recipient lifecycle
	// (channels.ErrMailboxOperation in the old tree; the messaging slice maps it).
	ErrPublicationNotMailbox = errors.New("channel publications are not mailbox items")
	// ErrRoutingReplyNotMailbox: the id is a routing reply.
	ErrRoutingReplyNotMailbox = errors.New("routing replies are not mailbox items")
)

// Store is the messaging tables' class C store.
type Store struct {
	all *store.Stores
	c   *store.Handle
	// Now is the store's clock (time.Now outside tests).
	Now func() time.Time
	// NewMessageID mints a message id (UUIDv7, as the old store).
	NewMessageID func() (string, error)
}

// New returns the store over the class handles.
func New(s *store.Stores) *Store {
	return &Store{all: s, c: s.C(), Now: time.Now, NewMessageID: func() (string, error) {
		id, err := uuid.NewV7()
		return id.String(), err
	}}
}

func (s *Store) now() time.Time { return s.Now().UTC() }

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func rebind(q gostore.Querier, query string) string { return q.Dialect().Rebind(query) }

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// pgUnNul is the Postgres expression for col (JSON text) with each unescaped \u0000 escape (one
// preceded by an even run of backslashes) replaced by \ufffd, so jsonb can hold it, while a literal
// backslash followed by "u0000" is left as it is. The pattern anchors on the character before the
// escape, so one pass misses the second of two adjacent escapes; the second pass catches it.
// (Look-behind would need "?", which Rebind reads as a placeholder.)
func pgUnNul(col string) string {
	const pattern = `'(^|[^\\])((\\\\)*)\\u0000', '\1\2\\ufffd', 'g'`
	return `regexp_replace(regexp_replace(` + col + `, ` + pattern + `), ` + pattern + `)`
}

// LockMessageSeq must run in every transaction that inserts into messages, before the insert. The
// mailbox watch cursor reads seq > after AND seq <= MAX(seq); on Postgres an identity value can
// commit after a higher one, so a watcher could pass a row forever. A transaction-scoped advisory
// lock makes message inserts commit in seq order (dialect audit decision 1, option (a); about 600
// messages a day). SQLite has one writer, so there it does nothing.
func LockMessageSeq(ctx context.Context, tx store.Tx) error {
	if tx.Dialect().Name() != gostore.DriverPostgres {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tether.messages.seq', 0))`)
	return err
}

// envelopeColumns is the SELECT list of a go-messaging envelope.
const envelopeColumns = `id, kind, channel, from_urn, to_urn, thread_id, in_reply_to, payload, content_type, metadata, created_at, delivered_at, consumed_at`

// messageColumns adds the recipient lifecycle.
const messageColumns = envelopeColumns + `, read_at, archived_at, canceled_at`

type scanFunc func(dest ...any) error

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t
}

func parseNullTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

// scanEnvelope reads envelopeColumns, then extra destinations.
func scanEnvelope(scan scanFunc, extra ...any) (messaging.Envelope, error) {
	var (
		env                                             messaging.Envelope
		kind, channel, from, to                         string
		threadID, inReplyTo, payload, contentType, meta sql.NullString
		created                                         string
		delivered, consumed                             sql.NullString
	)
	dest := append([]any{&env.ID, &kind, &channel, &from, &to, &threadID, &inReplyTo, &payload, &contentType, &meta,
		&created, &delivered, &consumed}, extra...)
	if err := scan(dest...); err != nil {
		return messaging.Envelope{}, err
	}
	env.Kind, env.Channel = messaging.Kind(kind), messaging.Channel(channel)
	env.ThreadID, env.InReplyTo, env.ContentType = threadID.String, inReplyTo.String, contentType.String
	var err error
	if env.From, err = messaging.ParseURN(from); err != nil {
		return messaging.Envelope{}, err
	}
	if env.To, err = messaging.ParseURN(to); err != nil {
		return messaging.Envelope{}, err
	}
	if payload.String != "" {
		env.Payload = json.RawMessage(payload.String)
	}
	if meta.String != "" {
		_ = json.Unmarshal([]byte(meta.String), &env.Metadata) // the old store ignored an unreadable metadata blob
	}
	env.CreatedAt = parseTime(created)
	env.DeliveredAt, env.ConsumedAt = parseNullTime(delivered), parseNullTime(consumed)
	return env, nil
}

// Message is an envelope with its recipient lifecycle and the projected subject and body.
type Message struct {
	messaging.Envelope
	ReadAt     *time.Time `json:"read_at"`
	ArchivedAt *time.Time `json:"archived_at"`
	CanceledAt *time.Time `json:"canceled_at"`
	Subject    string     `json:"subject,omitempty"`
	Body       string     `json:"body,omitempty"`
}

func scanMessage(scan scanFunc, extra ...any) (Message, error) {
	var read, archived, canceled sql.NullString
	env, err := scanEnvelope(scan, append([]any{&read, &archived, &canceled}, extra...)...)
	if err != nil {
		return Message{}, err
	}
	m := Message{Envelope: env, ReadAt: parseNullTime(read), ArchivedAt: parseNullTime(archived), CanceledAt: parseNullTime(canceled)}
	m.Subject, m.Body = ProjectPayload(m.Payload)
	return m, nil
}

// ProjectPayload derives a display subject and body from a payload: a JSON object's
// subject/title and body/summary/text/message strings, a JSON string's value, or the raw text.
func ProjectPayload(payload json.RawMessage) (subject, body string) {
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
		return firstString(obj, "subject", "title"), firstString(obj, "body", "summary", "text", "message")
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
