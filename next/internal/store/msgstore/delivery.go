package msgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/next/internal/store"
)

// This file is the storage half of the old delivery_store.go (S2b-2b part 2): go-messaging's
// delivery store over the library handle, and the SQL around it on Tether's messages rows. The
// deliveryBackedStore wrapper that composed the two (Send, Consume, MarkRead and Inbox with their
// receipts) is the messaging slice's (S6); it uses DeliveryStore, SendWithID, Consume,
// MailboxDeliveryID, SettleReadUndelivered and the pending receipt lease rows here.
//
// go-messaging's delivery store is SQLite-only (gap CW-20261115-0013): every library call, and every
// query of ours over its messaging_* tables, goes through Stores.LibraryDB, fetched with the ctx in
// hand just before the call, never inside a WriteTx or Joint callback. On Postgres those calls return
// store.ErrLibraryUnavailable. The SQL on Tether's own tables runs on both dialects.

// ErrNoDeliveryTracking is returned by ClaimMessageDelivery for a message with no delivery-core
// tracking (one that predates it: delivery_id is NULL), so there is nothing durable to claim.
var ErrNoDeliveryTracking = errors.New("delivery store: message has no delivery-core tracking")

// deliverySQLiteTimeFormat is the layout go-messaging's SQLite store writes, and compares as text,
// for next_attempt_at and deadline_at.
const deliverySQLiteTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// DeliveryStore returns go-messaging's delivery store over the library handle, for one call or one
// short sequence of calls. Fetch it with the ctx in hand right before the call and do not keep it:
// inside a WriteTx or Joint callback it fails (store.ErrNestedWrite), and on Postgres it returns
// store.ErrLibraryUnavailable.
func (s *Store) DeliveryStore(ctx context.Context) (*delivery.SQLiteStore, error) {
	db, err := s.all.LibraryDB(ctx)
	if err != nil {
		return nil, err
	}
	return delivery.NewSQLiteStore(db), nil
}

// ActivateDelivery is the old Open's delivery start-up: it creates or upgrades go-messaging's
// messaging_* tables (the library owns that schema) and fences pre-activation library history once,
// moving the 'delivery' event checkpoint from its -1 seed to the transitions' high water. Reopening
// never moves an established checkpoint (the write is guarded on -1). store.ErrLibraryUnavailable on
// Postgres, where the checkpoint stays -1 until the library has a store there.
func (s *Store) ActivateDelivery(ctx context.Context) error {
	db, err := s.all.LibraryDB(ctx)
	if err != nil {
		return err
	}
	if err := delivery.ApplySQLiteSchema(ctx, db); err != nil {
		return fmt.Errorf("apply delivery schema: %w", err)
	}
	var high int64
	if err := db.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM messaging_transitions`).Scan(&high); err != nil {
		return fmt.Errorf("activate message events: %w", err)
	}
	_, err = s.execC(ctx, `UPDATE message_event_checkpoints SET position = ? WHERE source = 'delivery' AND position = -1`, high)
	if err != nil {
		return fmt.Errorf("activate message events: %w", err)
	}
	return nil
}

// SetPendingReceiptLease records on the message the claim (attempt and lease token) whose receipts
// the recipient's Consume will settle (migration 0022's marker columns): the wake marks its own claim
// as Consume-owned right after claiming.
func (s *Store) SetPendingReceiptLease(ctx context.Context, messageID string, attemptID delivery.AttemptID, leaseToken delivery.LeaseToken) error {
	_, err := s.execC(ctx, `UPDATE messages SET pending_receipt_attempt_id = ?, pending_receipt_lease_token = ? WHERE id = ?`,
		string(attemptID), string(leaseToken), messageID)
	return err
}

// PendingReceiptLease returns the message's pending receipt claim ("" when none).
func (s *Store) PendingReceiptLease(ctx context.Context, messageID string) (attemptID, leaseToken string, err error) {
	r := s.c.Reader()
	var a, l sql.NullString
	if err := r.QueryRowContext(ctx, rebind(r, `SELECT pending_receipt_attempt_id, pending_receipt_lease_token FROM messages WHERE id = ?`),
		messageID).Scan(&a, &l); err != nil {
		return "", "", err
	}
	return a.String, l.String, nil
}

// MessageHandledByRecipient reports whether the recipient consumed the message or marked it read,
// the acts after which a wake would be a duplicate. delivered_at is not consulted: any listing of
// the mailbox stamps it, and a listing must not settle a delivery.
func (s *Store) MessageHandledByRecipient(ctx context.Context, messageID string) (bool, error) {
	r := s.c.Reader()
	var handled int64
	err := r.QueryRowContext(ctx, rebind(r, `SELECT CASE WHEN consumed_at IS NOT NULL OR read_at IS NOT NULL THEN 1 ELSE 0 END
 FROM messages WHERE id = ?`), messageID).Scan(&handled)
	if err != nil {
		return false, err
	}
	return handled == 1, nil
}

// DeliveryIDForMessage returns the message's delivery-core id. ok is false, with no error, for a
// message that predates delivery-core tracking (delivery_id NULL); an unknown message is
// messaging.ErrNotFound.
func (s *Store) DeliveryIDForMessage(ctx context.Context, messageID string) (string, bool, error) {
	r := s.c.Reader()
	var id sql.NullString
	err := r.QueryRowContext(ctx, rebind(r, `SELECT delivery_id FROM messages WHERE id = ?`), messageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("delivery id for message %s: %w", messageID, messaging.ErrNotFound)
	}
	if err != nil {
		return "", false, fmt.Errorf("delivery id for message %s: %w", messageID, err)
	}
	if !id.Valid || id.String == "" {
		return "", false, nil
	}
	return id.String, true, nil
}

// MailboxDeliveryID returns the delivery id of a mailbox message (not a staged turn output), ""
// when it has none: the old MarkRead's lookup before it recorded the consumed receipts.
func (s *Store) MailboxDeliveryID(ctx context.Context, messageID string) (string, error) {
	r := s.c.Reader()
	var id sql.NullString
	err := r.QueryRowContext(ctx, rebind(r, `SELECT delivery_id FROM messages WHERE id = ? AND routing_staged = 0`), messageID).Scan(&id)
	if err != nil {
		return "", err
	}
	return id.String, nil
}

// inboxSettleBatch bounds one SettleReadUndelivered pass.
const inboxSettleBatch = 500

// SettledRead is a read message SettleReadUndelivered stamped delivered.
type SettledRead struct {
	MessageID, DeliveryID string
}

// SettleReadUndelivered stamps delivered_at on the recipient's read but undelivered messages (kind
// and thread narrowed by f), up to 500, oldest first, and returns those it stamped, so the caller
// records their consumed receipts (the old unread-only Inbox pull's settle). Only a row this call
// stamped is returned: on Postgres the rows are locked as they are read (SKIP LOCKED), so two
// concurrent pulls never both report one message.
func (s *Store) SettleReadUndelivered(ctx context.Context, to messaging.Address, f messaging.Filter) ([]SettledRead, error) {
	where := "routing_staged = 0 AND to_urn = ? AND delivered_at IS NULL AND canceled_at IS NULL AND read_at IS NOT NULL"
	args := []any{to.URN()}
	kinds, kargs := kindIn(f.Kind)
	where += kinds
	args = append(args, kargs...)
	if f.ThreadID != "" {
		where, args = where+" AND thread_id = ?", append(args, f.ThreadID)
	}
	var out []SettledRead
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		out = nil
		lock := ""
		if tx.Dialect().Name() == gostore.DriverPostgres {
			lock = " FOR UPDATE SKIP LOCKED"
		}
		rows, err := tx.QueryContext(ctx, rebind(tx, `SELECT id, delivery_id FROM messages WHERE `+where+` ORDER BY created_at, id LIMIT ?`+lock),
			append(args, inboxSettleBatch)...)
		if err != nil {
			return err
		}
		var found []SettledRead
		for rows.Next() {
			var m SettledRead
			var deliveryID sql.NullString
			if err := rows.Scan(&m.MessageID, &deliveryID); err != nil {
				_ = rows.Close()
				return err
			}
			m.DeliveryID = deliveryID.String
			found = append(found, m)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		now := stamp(s.now())
		for _, m := range found {
			res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE messages SET delivered_at = ? WHERE id = ? AND delivered_at IS NULL AND read_at IS NOT NULL`), now, m.MessageID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 1 {
				out = append(out, m)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("delivery store: settle read messages for %s: %w", to.URN(), err)
	}
	return out, nil
}

// ClaimMessageDelivery durably claims message id's delivery for an external holder (a
// published-local bridge) on its own initiative. recipient must be the message's To (the
// self-asserted convention Consume uses): ErrWrongRecipient otherwise, ErrNoDeliveryTracking for a
// message without delivery-core tracking. The lease is the bearer credential the holder presents to
// Ack/NackMessageDelivery.
func (s *Store) ClaimMessageDelivery(ctx context.Context, id string, recipient messaging.Address, holder string, leaseDuration time.Duration) (messaging.Envelope, delivery.LeaseRef, error) {
	env, err := s.Get(ctx, id)
	if err != nil {
		return messaging.Envelope{}, delivery.LeaseRef{}, err
	}
	if env.To.URN() != recipient.URN() {
		return messaging.Envelope{}, delivery.LeaseRef{}, ErrWrongRecipient
	}
	deliveryID, ok, err := s.DeliveryIDForMessage(ctx, id)
	if err != nil {
		return messaging.Envelope{}, delivery.LeaseRef{}, err
	}
	if !ok {
		return messaging.Envelope{}, delivery.LeaseRef{}, ErrNoDeliveryTracking
	}
	ds, err := s.DeliveryStore(ctx)
	if err != nil {
		return messaging.Envelope{}, delivery.LeaseRef{}, err
	}
	claim, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: delivery.DeliveryID(deliveryID), Holder: holder, LeaseDuration: leaseDuration, Nowait: true})
	if err != nil {
		return messaging.Envelope{}, delivery.LeaseRef{}, err
	}
	return env, delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID,
		LeaseToken: claim.Attempt.LeaseToken, BindingGeneration: claim.Attempt.BindingGeneration}, nil
}

// AckMessageDelivery records stage against the lease.
func (s *Store) AckMessageDelivery(ctx context.Context, lease delivery.LeaseRef, stage delivery.ReceiptStage) (delivery.RecipientDelivery, delivery.Attempt, error) {
	ds, err := s.DeliveryStore(ctx)
	if err != nil {
		return delivery.RecipientDelivery{}, delivery.Attempt{}, err
	}
	return ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: stage})
}

// NackMessageDelivery records a failed or declined attempt against the lease; nextAttemptIn > 0
// schedules the retry that far from now.
func (s *Store) NackMessageDelivery(ctx context.Context, lease delivery.LeaseRef, retryable bool, errMsg string, nextAttemptIn time.Duration) (delivery.RecipientDelivery, delivery.Attempt, error) {
	var nextAttemptAt time.Time
	if nextAttemptIn > 0 {
		nextAttemptAt = s.now().Add(nextAttemptIn)
	}
	ds, err := s.DeliveryStore(ctx)
	if err != nil {
		return delivery.RecipientDelivery{}, delivery.Attempt{}, err
	}
	return ds.Nack(ctx, delivery.NackRequest{Lease: lease, Retryable: retryable, Error: errMsg, NextAttemptAt: nextAttemptAt})
}

// ReadyPage is one slice of the wake sweep's ready queue. LastID is the id of the last row the SQL
// page returned (rows skipped as unusable included), and Exhausted reports that the page came back
// short (the queue ended, at upToID when given), so the caller decides when a scan cycle is over.
type ReadyPage struct {
	Rows      []delivery.RecipientDelivery
	LastID    delivery.DeliveryID
	Exhausted bool
}

// DeliveryHighWater returns the greatest delivery id in any state ("" when there are none): a sweep
// cycle's upper bound, so a cycle covers a fixed set of rows.
func (s *Store) DeliveryHighWater(ctx context.Context) (delivery.DeliveryID, error) {
	db, err := s.all.LibraryDB(ctx)
	if err != nil {
		return "", err
	}
	var id sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT MAX(id) FROM messaging_deliveries`).Scan(&id); err != nil {
		return "", fmt.Errorf("delivery high water: %w", err)
	}
	return delivery.DeliveryID(id.String), nil
}

// ReadyDeliveryPage returns up to limit wake-sweep candidates (pending or retry_scheduled, backoff
// and deadline allowing a claim now) with id after afterID (and at most upToID when set), oldest
// first; delivery.Filter has no cursor, so a sweep reading only the oldest N could never pass N
// undeliverable rows. Only ID, MessageID, Recipient and Status are set. It first runs a one-row ready
// listing, which makes the library release lapsed leases, so expired leases are eligible here as in
// a plain listing.
func (s *Store) ReadyDeliveryPage(ctx context.Context, afterID, upToID delivery.DeliveryID, limit int) (ReadyPage, error) {
	if limit <= 0 {
		return ReadyPage{Exhausted: true}, nil
	}
	db, err := s.all.LibraryDB(ctx)
	if err != nil {
		return ReadyPage{}, err
	}
	if _, err := delivery.NewSQLiteStore(db).ListDeliveries(ctx, delivery.Filter{ReadyOnly: true, Limit: 1}); err != nil {
		return ReadyPage{}, fmt.Errorf("ready delivery page: release expired leases: %w", err)
	}
	now := s.now().Format(deliverySQLiteTimeFormat)
	query := `SELECT id, message_id, recipient_urn, status FROM messaging_deliveries
 WHERE id > ? AND status IN (?, ?) AND (next_attempt_at IS NULL OR next_attempt_at <= ?) AND (deadline_at IS NULL OR deadline_at > ?)`
	args := []any{string(afterID), string(delivery.DeliveryPending), string(delivery.DeliveryRetryScheduled), now, now}
	if upToID != "" {
		query, args = query+` AND id <= ?`, append(args, string(upToID))
	}
	query, args = query+` ORDER BY id ASC LIMIT ?`, append(args, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return ReadyPage{}, fmt.Errorf("ready delivery page: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page, raw := ReadyPage{}, 0
	for rows.Next() {
		var id, msgID, urn, status string
		if err := rows.Scan(&id, &msgID, &urn, &status); err != nil {
			return ReadyPage{}, fmt.Errorf("ready delivery page: scan: %w", err)
		}
		raw++
		page.LastID = delivery.DeliveryID(id)
		to, err := messaging.ParseURN(urn)
		if err != nil {
			continue // unusable as an address: skipped from Rows, still counted in LastID and Exhausted
		}
		page.Rows = append(page.Rows, delivery.RecipientDelivery{ID: delivery.DeliveryID(id), MessageID: delivery.MessageID(msgID), Recipient: to, Status: delivery.DeliveryStatus(status)})
	}
	if err := rows.Err(); err != nil {
		return ReadyPage{}, fmt.Errorf("ready delivery page: %w", err)
	}
	page.Exhausted = raw < limit
	return page, nil
}
