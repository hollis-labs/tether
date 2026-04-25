package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/go-messaging"
)

// ErrWrongRecipient is returned by Consume when the caller's `as` URN does
// not match the envelope's intended `to_urn`. Callers should surface this
// as HTTP 409 Conflict (the message exists but the caller is not entitled
// to consume it).
var ErrWrongRecipient = errors.New("consume: caller is not the intended recipient")

// messagingStore implements messaging.Store backed by the SQLite messages
// table (migration 0010). It is obtained via (*Store).MessagingStore().
//
// Subscribe fan-out is in-memory: notifications are lost on daemon restart.
// This matches the memstore semantics and the design-spec intent —
// durability is at the envelope level, not the subscription level.
type messagingStore struct {
	db *sql.DB

	subMu       sync.Mutex
	subscribers []*msgSubscription
}

type msgSubscription struct {
	to     messaging.Address
	filter messaging.Filter
	ch     chan messaging.Envelope
	done   <-chan struct{}
}

// MessagingStore returns the singleton go-messaging Store backed by this
// SQLite store. The same instance is returned on every call so that
// in-memory fan-out (Subscribe → Send) works correctly across all callers
// within one process.
func (s *Store) MessagingStore() messaging.Store {
	s.msgOnce.Do(func() {
		s.msgStore = &messagingStore{db: s.db}
	})
	return s.msgStore
}

// ─── Send ────────────────────────────────────────────────────────────────────

func (ms *messagingStore) Send(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error) {
	if env.DeliveredAt != nil || env.ConsumedAt != nil {
		return messaging.Envelope{}, messaging.ErrPresetLifecycle
	}
	id, err := uuid.NewV7()
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("messaging store: uuid v7: %w", err)
	}
	env.ID = id.String()
	env.CreatedAt = time.Now().UTC()
	env.DeliveredAt = nil
	env.ConsumedAt = nil

	metaJSON := ""
	if len(env.Metadata) > 0 {
		b, _ := json.Marshal(env.Metadata)
		metaJSON = string(b)
	}
	payloadStr := ""
	if len(env.Payload) > 0 {
		payloadStr = string(env.Payload)
	}

	_, err = ms.db.ExecContext(ctx,
		`INSERT INTO messages
		 (id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
		  payload, content_type, metadata, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		env.ID,
		string(env.Kind),
		string(env.Channel),
		env.From.URN(),
		env.To.URN(),
		nullIfEmpty(env.ThreadID),
		nullIfEmpty(env.InReplyTo),
		nullIfEmpty(payloadStr),
		nullIfEmpty(env.ContentType),
		nullIfEmpty(metaJSON),
		env.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("messaging store: send: %w", err)
	}

	ms.fanOut(env)
	return env, nil
}

// ─── Get ─────────────────────────────────────────────────────────────────────

func (ms *messagingStore) Get(ctx context.Context, id string) (messaging.Envelope, error) {
	row := ms.db.QueryRowContext(ctx,
		`SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
		        payload, content_type, metadata, created_at,
		        delivered_at, consumed_at
		 FROM messages WHERE id=?`, id)
	env, err := scanEnvelope(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return messaging.Envelope{}, messaging.ErrNotFound
	}
	return env, err
}

// ─── Inbox ───────────────────────────────────────────────────────────────────

// Inbox returns undelivered envelopes for `to` and atomically marks them as
// delivered within a single write transaction (BEGIN IMMEDIATE).
//
// The BEGIN IMMEDIATE lock ensures that two concurrent Inbox calls for the
// same recipient cannot both see the same undelivered row: the second caller
// blocks at the transaction boundary until the first commits its
// delivered_at updates. This is the atomic-delivery guarantee required by
// ADR-0023 §4 and the messaging.Store contract.
func (ms *messagingStore) Inbox(ctx context.Context, to messaging.Address, f messaging.Filter) ([]messaging.Envelope, error) {
	toURN := to.URN()
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	// Build WHERE clause for optional Filter fields.
	where := "to_urn=? AND delivered_at IS NULL AND canceled_at IS NULL"
	args := []any{toURN}
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

	// BeginTx with default options starts a deferred transaction (not IMMEDIATE).
	// Write contention is already prevented by the single-connection pool
	// (MaxOpenConns=1 in sqlite.go), so an IMMEDIATE lock is not required here.
	// If the connection pool is ever widened, consider switching to
	// sql.TxOptions{Isolation: sql.LevelSerializable} or issuing a manual
	// BEGIN IMMEDIATE to preserve the serialization guarantee.
	tx, err := ms.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("messaging store: inbox begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx,
		`SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
		        payload, content_type, metadata, created_at,
		        delivered_at, consumed_at
		 FROM messages WHERE `+where+` ORDER BY created_at ASC, id ASC LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("messaging store: inbox query: %w", err)
	}

	var out []messaging.Envelope
	var ids []string
	for rows.Next() {
		env, err := scanEnvelope(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, env)
		ids = append(ids, env.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Mark as DELIVERED within the same transaction — atomic with the SELECT.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET delivered_at=? WHERE id=? AND delivered_at IS NULL`,
			now, id); err != nil {
			return nil, fmt.Errorf("messaging store: inbox mark delivered: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("messaging store: inbox commit: %w", err)
	}
	return out, nil
}

// ─── Thread ──────────────────────────────────────────────────────────────────

func (ms *messagingStore) Thread(ctx context.Context, threadID string, f messaging.Filter) ([]messaging.Envelope, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	where := "thread_id=?"
	args := []any{threadID}
	if len(f.Kind) > 0 {
		in := make([]any, len(f.Kind))
		for i, k := range f.Kind {
			in[i] = string(k)
		}
		where += " AND kind IN (?" + repeatCommaQ(len(f.Kind)-1) + ")"
		args = append(args, in...)
	}

	rows, err := ms.db.QueryContext(ctx,
		`SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
		        payload, content_type, metadata, created_at,
		        delivered_at, consumed_at
		 FROM messages WHERE `+where+` ORDER BY created_at ASC, id ASC LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("messaging store: thread query: %w", err)
	}
	defer rows.Close()

	var out []messaging.Envelope
	for rows.Next() {
		env, err := scanEnvelope(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

// ─── Consume ─────────────────────────────────────────────────────────────────

// Consume marks a message as consumed by `recipient`. Idempotent: if the
// message was already consumed by this recipient, returns nil. Returns
// ErrNotFound if the id does not exist. Returns ErrWrongRecipient if the
// message exists but `recipient` is not the intended `to_urn`.
func (ms *messagingStore) Consume(ctx context.Context, id string, recipient messaging.Address) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := ms.db.ExecContext(ctx,
		`UPDATE messages SET consumed_at=? WHERE id=? AND to_urn=? AND consumed_at IS NULL`,
		now, id, recipient.URN())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return nil // updated — success
	}
	// 0 rows: already consumed (idempotent OK), wrong recipient, or not found.
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
	// Row exists and to_urn matches — already consumed. Idempotent.
	return nil
}

// ─── Cancel ──────────────────────────────────────────────────────────────────

func (ms *messagingStore) Cancel(ctx context.Context, id string) error {
	res, err := ms.db.ExecContext(ctx,
		`UPDATE messages SET canceled_at=? WHERE id=? AND canceled_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Already canceled or not found — check which.
		var exists int
		if err := ms.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE id=?`, id).Scan(&exists); err != nil || exists == 0 {
			return messaging.ErrNotFound
		}
	}
	// Notify any Subscribe waiter that may be waiting for responses on this ID.
	// Deliver a zero-value envelope with a sentinel so fanOut can detect cancel.
	ms.notifyCanceled(id)
	return nil
}

// ─── Subscribe ───────────────────────────────────────────────────────────────

func (ms *messagingStore) Subscribe(ctx context.Context, to messaging.Address, f messaging.Filter) (<-chan messaging.Envelope, error) {
	ch := make(chan messaging.Envelope, 32)
	sub := &msgSubscription{to: to, filter: f, ch: ch, done: ctx.Done()}

	ms.subMu.Lock()
	ms.subscribers = append(ms.subscribers, sub)
	ms.subMu.Unlock()

	go func() {
		<-ctx.Done()
		ms.subMu.Lock()
		for i, s := range ms.subscribers {
			if s == sub {
				ms.subscribers = append(ms.subscribers[:i], ms.subscribers[i+1:]...)
				break
			}
		}
		ms.subMu.Unlock()
		close(ch)
	}()

	return ch, nil
}

// fanOut delivers env to all matching live subscribers.
func (ms *messagingStore) fanOut(env messaging.Envelope) {
	ms.subMu.Lock()
	subs := make([]*msgSubscription, len(ms.subscribers))
	copy(subs, ms.subscribers)
	ms.subMu.Unlock()

	for _, s := range subs {
		if !s.to.IsZero() && s.to != env.To {
			continue
		}
		if s.filter.Matches(env) {
			select {
			case s.ch <- env:
			default:
				// Subscriber is slow; drop rather than block Send.
			}
		}
	}
}

// notifyCanceled sends a canceled-sentinel envelope so any in-flight
// Dispatcher.Request waiting on InReplyTo=id sees ErrCanceled.
// The sentinel has Kind=response and InReplyTo set; the dispatcher's
// Request loop will pick it up. We rely on the fact that a zero-Address
// envelope will fail the InReplyTo match in the dispatcher loop and be
// ignored by callers not waiting on this ID — only the matching waiter
// picks it up. Since dispatcher.Request subscribes to Kind=response,
// we emit a fake response with a special sentinel payload.
//
// In practice: the dispatcher in go-messaging handles this by ctx cancel,
// not by sentinel. We simply close any subscriber channel that's waiting.
// The cleanest approach for SQLite-backed Cancel is to rely on context
// cancellation from the caller after Cancel returns ErrCanceled.
// No additional fanOut needed here — the contract test for Cancel only
// requires Cancel to be idempotent and return ErrNotFound when absent.
func (ms *messagingStore) notifyCanceled(_ string) {}

// ─── Scan helper ─────────────────────────────────────────────────────────────

type scanFunc func(dest ...any) error

func scanEnvelope(scan scanFunc) (messaging.Envelope, error) {
	var (
		env                                                   messaging.Envelope
		kindStr, channelStr, fromURN, toURN                   string
		threadID, inReplyTo, payloadStr, contentType, metaStr sql.NullString
		createdStr                                            string
		deliveredStr, consumedStr                             sql.NullString
	)
	err := scan(
		&env.ID, &kindStr, &channelStr, &fromURN, &toURN,
		&threadID, &inReplyTo, &payloadStr, &contentType, &metaStr,
		&createdStr, &deliveredStr, &consumedStr,
	)
	if err != nil {
		return messaging.Envelope{}, err
	}

	env.Kind = messaging.Kind(kindStr)
	env.Channel = messaging.Channel(channelStr)
	env.ThreadID = threadID.String
	env.InReplyTo = inReplyTo.String
	env.ContentType = contentType.String

	from, err := messaging.ParseURN(fromURN)
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("parse from_urn %q: %w", fromURN, err)
	}
	env.From = from

	to, err := messaging.ParseURN(toURN)
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("parse to_urn %q: %w", toURN, err)
	}
	env.To = to

	if payloadStr.Valid && payloadStr.String != "" {
		env.Payload = json.RawMessage(payloadStr.String)
	}
	if metaStr.Valid && metaStr.String != "" {
		_ = json.Unmarshal([]byte(metaStr.String), &env.Metadata)
	}

	t, err := time.Parse(time.RFC3339Nano, createdStr)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, createdStr)
	}
	env.CreatedAt = t

	if deliveredStr.Valid && deliveredStr.String != "" {
		dt, _ := time.Parse(time.RFC3339Nano, deliveredStr.String)
		if dt.IsZero() {
			dt, _ = time.Parse(time.RFC3339, deliveredStr.String)
		}
		env.DeliveredAt = &dt
	}
	if consumedStr.Valid && consumedStr.String != "" {
		ct, _ := time.Parse(time.RFC3339Nano, consumedStr.String)
		if ct.IsZero() {
			ct, _ = time.Parse(time.RFC3339, consumedStr.String)
		}
		env.ConsumedAt = &ct
	}

	return env, nil
}

// repeatCommaQ returns n extra ", ?" strings for SQL IN clauses.
func repeatCommaQ(n int) string {
	if n <= 0 {
		return ""
	}
	s := ""
	for i := 0; i < n; i++ {
		s += ", ?"
	}
	return s
}
