package msgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
)

// Send stores a new message: it mints the id (UUIDv7) and created_at, as the old Send did. A
// preset delivered_at or consumed_at is refused (messaging.ErrPresetLifecycle). publication also
// records the channel publication row in the same transaction; deciding that a message is a
// publication, and that the sender may publish, is the channel slice's.
func (s *Store) Send(ctx context.Context, env messaging.Envelope, publication bool) (messaging.Envelope, error) {
	if env.DeliveredAt != nil || env.ConsumedAt != nil {
		return messaging.Envelope{}, messaging.ErrPresetLifecycle
	}
	id, err := s.NewMessageID()
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("messaging store: message id: %w", err)
	}
	env.ID, env.CreatedAt = id, s.now()
	return s.insert(ctx, env, "", publication)
}

// SendWithID stores a message whose id the caller fixed (the delivery-backed path, where the
// delivery library minted it), with its delivery id. A zero created_at is now.
func (s *Store) SendWithID(ctx context.Context, env messaging.Envelope, deliveryID string, publication bool) (messaging.Envelope, error) {
	if env.ID == "" {
		return messaging.Envelope{}, fmt.Errorf("messaging store: SendWithID: env.ID required")
	}
	if env.CreatedAt.IsZero() {
		env.CreatedAt = s.now()
	}
	return s.insert(ctx, env, deliveryID, publication)
}

func (s *Store) insert(ctx context.Context, env messaging.Envelope, deliveryID string, publication bool) (messaging.Envelope, error) {
	env.DeliveredAt, env.ConsumedAt = nil, nil
	meta := ""
	if len(env.Metadata) > 0 {
		b, err := json.Marshal(env.Metadata)
		if err != nil {
			return messaging.Envelope{}, err
		}
		meta = string(b)
	}
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := LockMessageSeq(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO messages
 (id, kind, channel, from_urn, to_urn, thread_id, in_reply_to, payload, content_type, metadata, created_at, delivery_id)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			env.ID, string(env.Kind), string(env.Channel), env.From.URN(), env.To.URN(), nullText(env.ThreadID), nullText(env.InReplyTo),
			nullText(string(env.Payload)), nullText(env.ContentType), nullText(meta), stamp(env.CreatedAt), nullText(deliveryID)); err != nil {
			return fmt.Errorf("messaging store: send: %w", err)
		}
		if publication {
			_, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO channel_publications (name, message_id) VALUES (?, ?)`), string(env.Channel), env.ID)
			return err
		}
		return nil
	})
	if err != nil {
		return messaging.Envelope{}, err
	}
	return env, nil
}

// Get returns a non-staged message.
func (s *Store) Get(ctx context.Context, id string) (messaging.Envelope, error) {
	r := s.c.Reader()
	env, err := scanEnvelope(r.QueryRowContext(ctx, rebind(r, `SELECT `+envelopeColumns+` FROM messages WHERE id = ? AND routing_staged = 0`), id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return messaging.Envelope{}, messaging.ErrNotFound
	}
	return env, err
}

func kindIn(kinds []messaging.Kind) (string, []any) {
	if len(kinds) == 0 {
		return "", nil
	}
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = string(k)
	}
	return " AND kind IN (" + placeholders(len(kinds)) + ")", args
}

func scanEnvelopes(rows *sql.Rows) ([]messaging.Envelope, error) {
	defer func() { _ = rows.Close() }()
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

// Thread returns a thread's non-staged messages oldest first (limit default 100).
func (s *Store) Thread(ctx context.Context, threadID string, f messaging.Filter) ([]messaging.Envelope, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	kinds, kargs := kindIn(f.Kind)
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT `+envelopeColumns+` FROM messages
 WHERE routing_staged = 0 AND thread_id = ?`+kinds+` ORDER BY created_at ASC, id ASC LIMIT ?`), append(append([]any{threadID}, kargs...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("messaging store: thread query: %w", err)
	}
	return scanEnvelopes(rows)
}

// Inbox returns the recipient's undelivered, uncanceled messages oldest first and marks them
// delivered in the same transaction (limit default 100). unreadOnly also skips read messages.
func (s *Store) Inbox(ctx context.Context, to messaging.Address, f messaging.Filter, unreadOnly bool) ([]messaging.Envelope, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	where := "routing_staged = 0 AND to_urn = ? AND delivered_at IS NULL AND canceled_at IS NULL"
	if unreadOnly {
		where += " AND read_at IS NULL"
	}
	args := []any{to.URN()}
	kinds, kargs := kindIn(f.Kind)
	where += kinds
	args = append(args, kargs...)
	if f.ThreadID != "" {
		where, args = where+" AND thread_id = ?", append(args, f.ThreadID)
	}
	var out []messaging.Envelope
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// Deliver-once: on Postgres (READ COMMITTED, concurrent writers) the rows are locked as they are
		// read and rows another Inbox holds are skipped; on both dialects only a row this call marked
		// delivered is returned, so a message is never handed to two callers.
		lock := ""
		if tx.Dialect().Name() == gostore.DriverPostgres {
			lock = " FOR UPDATE SKIP LOCKED"
		}
		rows, err := tx.QueryContext(ctx, rebind(tx, `SELECT `+envelopeColumns+` FROM messages WHERE `+where+` ORDER BY created_at ASC, id ASC LIMIT ?`+lock), append(args, limit)...)
		if err != nil {
			return fmt.Errorf("messaging store: inbox query: %w", err)
		}
		selected, err := scanEnvelopes(rows)
		if err != nil {
			return err
		}
		out = make([]messaging.Envelope, 0, len(selected))
		now := stamp(s.now())
		for _, env := range selected {
			res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE messages SET delivered_at = ? WHERE id = ? AND delivered_at IS NULL`), now, env.ID)
			if err != nil {
				return fmt.Errorf("messaging store: inbox mark delivered: %w", err)
			}
			if n, err := res.RowsAffected(); err != nil {
				return fmt.Errorf("messaging store: inbox mark delivered: %w", err)
			} else if n == 1 {
				out = append(out, env)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RequireMailbox refuses an id that is a channel publication or a routing reply: neither has a
// recipient lifecycle.
func (s *Store) RequireMailbox(ctx context.Context, id string) error {
	r := s.c.Reader()
	var publication, reply bool
	if err := r.QueryRowContext(ctx, rebind(r, `SELECT EXISTS(SELECT 1 FROM channel_publications WHERE message_id = ?),
 EXISTS(SELECT 1 FROM routing_replies WHERE reply_id = ?)`), id, id).Scan(&publication, &reply); err != nil {
		return err
	}
	switch {
	case publication:
		return ErrPublicationNotMailbox
	case reply:
		return ErrRoutingReplyNotMailbox
	}
	return nil
}

// recipientOf tells "not found" from "someone else's" after an update matched nothing.
func (s *Store) recipientOf(ctx context.Context, id string, recipient messaging.Address) error {
	r := s.c.Reader()
	var to string
	err := r.QueryRowContext(ctx, rebind(r, `SELECT to_urn FROM messages WHERE id = ? AND routing_staged = 0`), id).Scan(&to)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return messaging.ErrNotFound
	case err != nil:
		return err
	case to != recipient.URN():
		return ErrWrongRecipient
	}
	return nil
}

// Consume marks a message consumed by its recipient and returns its delivery id ("" when it has
// none). A repeat by the recipient succeeds; another caller gets ErrWrongRecipient.
func (s *Store) Consume(ctx context.Context, id string, recipient messaging.Address) (deliveryID string, err error) {
	if err := s.RequireMailbox(ctx, id); err != nil {
		return "", err
	}
	var n int64
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE messages SET consumed_at = ? WHERE routing_staged = 0 AND id = ? AND to_urn = ? AND consumed_at IS NULL`),
			stamp(s.now()), id, recipient.URN())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return "", err
	}
	if n == 0 {
		if err := s.recipientOf(ctx, id, recipient); err != nil {
			return "", err
		}
	}
	r := s.c.Reader()
	var delivery sql.NullString
	if err := r.QueryRowContext(ctx, rebind(r, `SELECT delivery_id FROM messages WHERE id = ? AND routing_staged = 0`), id).Scan(&delivery); err != nil {
		return "", nil //nolint:nilerr // the consume succeeded; the old store treated the delivery id read-back as best effort
	}
	return delivery.String, nil
}

// Cancel marks a message canceled (idempotent); a missing id is messaging.ErrNotFound.
func (s *Store) Cancel(ctx context.Context, id string) error {
	if err := s.RequireMailbox(ctx, id); err != nil {
		return err
	}
	var n int64
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE messages SET canceled_at = ? WHERE routing_staged = 0 AND id = ? AND canceled_at IS NULL`), stamp(s.now()), id)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil || n > 0 {
		return err
	}
	r := s.c.Reader()
	var exists int
	if err := r.QueryRowContext(ctx, rebind(r, `SELECT COUNT(*) FROM messages WHERE id = ? AND routing_staged = 0`), id).Scan(&exists); err != nil || exists == 0 {
		return messaging.ErrNotFound
	}
	return nil
}
