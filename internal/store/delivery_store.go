package store

// delivery_store.go — T03 (messaging vNext, CW-20260904-0100). Adopts
// go-messaging's reliable-delivery core (github.com/hollis-labs/go-messaging/
// delivery) as the one authoritative recipient-obligation/attempt/receipt
// tracker, replacing the hand-rolled delivered_at/consumed_at state machine
// in messaging_store.go for that specific concern.
//
// Division of authority (T01 contract §3.2, CONTRACTS.md's four-way split):
//   - `messages` (this package, unchanged) stays the sole source of truth
//     for MESSAGE CONTENT (body/thread/reply/metadata) and ATTENTION state
//     (read_at/archived_at, via messaging_inbox.go). Get/Inbox/Thread/List/
//     MarkRead/Archive/Unarchive/Subscribe are untouched by this file --
//     zero behavior change for any existing caller.
//   - go-messaging's messaging_* tables (delivery.SQLiteStore) become the
//     sole source of truth for RECIPIENT DELIVERY OBLIGATIONS, ATTEMPTS and
//     RECEIPTS -- the thing delivered_at/consumed_at were a weak, crash-
//     unsafe proxy for (T01 §2.4.1: no automatic redelivery if a consumer
//     crashes between Inbox's delivered_at stamp and Consume).
//
// deliveryBackedStore wraps the existing InboxStore and overrides exactly
// the two methods that change delivery lifecycle state (Send, Consume).
// Get/Inbox/Thread/List/MarkRead/Archive/Unarchive/Subscribe/Cancel pass
// through unchanged. Cancel is intentionally NOT wired to the delivery core:
// the delivery.Store interface has no cancel/delete primitive (by design --
// CONTRACTS.md's reliable-delivery stages are persisted/lease_acquired/
// host_accepted/turn_submitted/consumed/failed/dead_lettered/canceled, but
// "canceled" there means a claimed-and-then-aborted attempt, not a sender
// aborting an unclaimed message pre-delivery). A canceled message's delivery
// obligation, if one was created, is simply never claimed -- harmless, since
// nothing yet reads FROM the delivery core as a primary path (that's T06's
// pump). Documented here rather than worked around with raw SQL against the
// library's private schema.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
)

// deliveryConsumeLeaseDuration is long enough to cover the immediate
// Claim-then-Ack sequence Consume performs (no real "held over time" lease
// use here -- see recordConsumedReceipts) but short enough that a crash
// between Claim and Ack doesn't tie up the obligation for long before it's
// eligible for reclaim.
const deliveryConsumeLeaseDuration = 30 * time.Second

// deliveryBackedStore decorates *messagingStore with go-messaging
// delivery-core bookkeeping. Constructed by (*Store).MessagingStore();
// not exported -- callers depend on the InboxStore interface, not this
// concrete type. Embeds the concrete *messagingStore (not just the
// InboxStore interface) so it can reach the two package-private helper
// methods (sendWithID, consumeAndReportTransition) that exist solely to
// support this decorator without widening the public InboxStore contract
// every other implementer/stub would have to satisfy.
type deliveryBackedStore struct {
	*messagingStore
	delivery delivery.Store
}

var _ InboxStore = (*deliveryBackedStore)(nil)

// Send persists the envelope via the same content path as before (mints the
// message ID by calling the delivery core FIRST and reusing its minted
// message ID for the `messages` row -- see below), then creates a real,
// durable recipient delivery obligation in the delivery core.
//
// Ordering and failure mode: delivery.Enqueue runs first (its own internal
// transaction). Only on success does the `messages` INSERT happen (reusing
// Enqueue's minted Message.ID as messages.id, and recording the resulting
// RecipientDelivery.ID in messages.delivery_id). If the `messages` INSERT
// then fails, the delivery-core row is orphaned (no corresponding content
// row) -- an extremely narrow, catastrophic-failure-only window (the INSERT
// is simple and well-tested; this is not a routine failure mode). The
// delivery.Store interface exposes no cancel/delete primitive to compensate
// automatically, and reaching into the library's private schema to force
// one would violate "adopt the library rather than hand-roll a parallel
// implementation" -- so this failure is logged with the orphaned delivery ID
// for manual/T09 operator cleanup rather than silently swallowed, and Send
// still returns the error to the caller (never a false success).
//
// The reverse ordering (messages insert first, Enqueue second) was
// considered and rejected: it would orphan a content row with NO delivery
// tracking on Enqueue failure, silently defeating "adopt the shared delivery
// store as authoritative" for that message -- worse, because nothing would
// ever surface the gap (a delivery-core orphan is at least an orphan in the
// NEW system this task is standing up, more likely to be swept by future
// tooling than a silently under-tracked `messages` row would be).
//
// A single shared *sql.Tx spanning both Enqueue and the `messages` INSERT
// was also considered and rejected: delivery.Store.Enqueue manages its own
// internal transaction via the shared *sql.DB, and Tether's DB is configured
// with MaxOpenConns=1 (internal/store/sqlite.go) -- holding an outer
// transaction open while calling Enqueue (which needs to acquire the same
// single connection for its own BeginTx) would deadlock.
func (d *deliveryBackedStore) Send(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error) {
	if env.DeliveredAt != nil || env.ConsumedAt != nil {
		return messaging.Envelope{}, messaging.ErrPresetLifecycle
	}

	req, err := delivery.EnvelopeEnqueueRequest(env)
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("delivery store: project envelope: %w", err)
	}
	res, err := d.delivery.Enqueue(ctx, req)
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("delivery store: enqueue: %w", err)
	}
	if len(res.Deliveries) != 1 {
		return messaging.Envelope{}, fmt.Errorf("delivery store: enqueue: expected exactly 1 delivery for a single-recipient send, got %d", len(res.Deliveries))
	}

	env.ID = string(res.Message.ID)
	env.CreatedAt = res.Message.CreatedAt

	sent, err := d.sendWithID(ctx, env, string(res.Deliveries[0].ID))
	if err != nil {
		log.Printf("delivery store: messages insert failed after successful enqueue -- orphaned delivery %s (message %s) needs manual reconciliation: %v",
			res.Deliveries[0].ID, res.Message.ID, err)
		return messaging.Envelope{}, err
	}
	return sent, nil
}

// Consume drives the same idempotent, recipient-scoped consumed_at update
// as before (delegated to the wrapped InboxStore), then -- ONLY on the call
// that actually transitions consumed_at from NULL to set (never on the
// idempotent no-op replay) -- claims and acknowledges the corresponding
// delivery through host_accepted/turn_submitted/consumed in one immediate
// sequence. Tether's Consume has no separate "I'm now processing" signal
// distinct from "I finished processing" (T02 design research), so all three
// stages are recorded together rather than held open as a real lease.
//
// Delivery-core recording here is best-effort: if the message predates T03
// (no delivery_id recorded) or the delivery-core call fails for any reason
// (including a genuine race against another claimant), Consume still
// succeeds from the caller's perspective -- the `messages.consumed_at`
// update is Consume's core, tested contract; delivery-core receipt tracking
// is an additive enhancement layered on top, consistent with how this
// codebase already treats other enhancement-only writes (e.g.
// SetClaudeSessionID/UpsertSessionProviderMapping failures are logged, not
// propagated).
func (d *deliveryBackedStore) Consume(ctx context.Context, id string, recipient messaging.Address) error {
	transitioned, deliveryID, err := d.consumeAndReportTransition(ctx, id, recipient)
	if err != nil {
		return err
	}
	if !transitioned || deliveryID == "" {
		return nil
	}
	d.recordConsumedReceipts(ctx, deliveryID, recipient)
	return nil
}

func (d *deliveryBackedStore) recordConsumedReceipts(ctx context.Context, deliveryID string, recipient messaging.Address) {
	claim, err := d.delivery.Claim(ctx, delivery.ClaimRequest{
		DeliveryID:    delivery.DeliveryID(deliveryID),
		Holder:        recipient.URN(),
		LeaseDuration: deliveryConsumeLeaseDuration,
		Nowait:        true,
	})
	if err != nil {
		log.Printf("delivery store: consume: claim delivery %s failed (best-effort receipt recording skipped): %v", deliveryID, err)
		return
	}
	lease := delivery.LeaseRef{
		DeliveryID:        claim.Attempt.DeliveryID,
		AttemptID:         claim.Attempt.ID,
		LeaseToken:        claim.Attempt.LeaseToken,
		BindingGeneration: claim.Attempt.BindingGeneration,
	}
	for _, stage := range []delivery.ReceiptStage{delivery.StageHostAccepted, delivery.StageTurnSubmitted, delivery.StageConsumed} {
		if _, _, err := d.delivery.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: stage}); err != nil {
			log.Printf("delivery store: consume: ack stage %s for delivery %s failed (best-effort): %v", stage, deliveryID, err)
			return
		}
	}
}

// DeliveryStore returns the singleton go-messaging delivery.Store backed by
// this Store's *sql.DB. Exposed for callers (tests, T06's wake pump) that
// need direct access to Claim/Ack/Nack/Redrive/Attempts/Receipts beyond what
// the InboxStore surface exposes.
func (s *Store) DeliveryStore() delivery.Store {
	s.deliveryOnce.Do(func() {
		s.deliveryStoreVal = delivery.NewSQLiteStore(s.db)
	})
	return s.deliveryStoreVal
}

// DeliveryIDForMessage reads back the delivery-core RecipientDelivery.ID
// associated with a `messages` row (T06, messaging vNext). Returns
// ok=false, not an error, when the message predates T03 (delivery_id is
// NULL) -- callers use this to distinguish "no delivery-core tracking
// exists for this message" (a legacy row; skip Claim/Ack bookkeeping) from
// a genuine lookup failure.
func (s *Store) DeliveryIDForMessage(ctx context.Context, messageID string) (string, bool, error) {
	var id sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT delivery_id FROM messages WHERE id=?`, messageID).Scan(&id)
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

// ErrNoDeliveryTracking is returned by ClaimMessageDelivery when the
// target message predates T03 (no delivery_id recorded) -- there is
// nothing durable to claim.
var ErrNoDeliveryTracking = errors.New("delivery store: message has no delivery-core tracking")

// ClaimMessageDelivery is the authorized-external-holder counterpart to
// AttemptWake's internal Claim call (T07, messaging vNext): a published-
// local bridge (or any other caller) durably claims message id's delivery
// on its own initiative, rather than Tether pushing a wake. recipient is
// checked against the message's actual To address using the same
// self-asserted convention as Consume (ADR 0045) -- this is a new
// OPERATION under the existing trust model, not a new trust tier.
// Returns ErrWrongRecipient if recipient doesn't match, and
// ErrNoDeliveryTracking for a legacy pre-T03 message. The returned
// delivery.LeaseRef is the bearer credential the caller must present back
// to AckMessageDelivery/NackMessageDelivery -- it is not looked up or
// re-derived server-side, exactly like a message/delivery id.
func (s *Store) ClaimMessageDelivery(ctx context.Context, id string, recipient messaging.Address, holder string, leaseDuration time.Duration) (messaging.Envelope, delivery.LeaseRef, error) {
	env, err := s.MessagingStore().Get(ctx, id)
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
	claim, err := s.DeliveryStore().Claim(ctx, delivery.ClaimRequest{
		DeliveryID:    delivery.DeliveryID(deliveryID),
		Holder:        holder,
		LeaseDuration: leaseDuration,
		Nowait:        true,
	})
	if err != nil {
		return messaging.Envelope{}, delivery.LeaseRef{}, err
	}
	lease := delivery.LeaseRef{
		DeliveryID:        claim.Attempt.DeliveryID,
		AttemptID:         claim.Attempt.ID,
		LeaseToken:        claim.Attempt.LeaseToken,
		BindingGeneration: claim.Attempt.BindingGeneration,
	}
	return env, lease, nil
}

// AckMessageDelivery records stage against lease -- the HTTP-exposed
// counterpart of the same delivery.Store.Ack calls AttemptWake and
// Consume already make internally.
func (s *Store) AckMessageDelivery(ctx context.Context, lease delivery.LeaseRef, stage delivery.ReceiptStage) (delivery.RecipientDelivery, delivery.Attempt, error) {
	return s.DeliveryStore().Ack(ctx, delivery.AckRequest{Lease: lease, Stage: stage})
}

// NackMessageDelivery records a failed/declined attempt against lease --
// the HTTP-exposed counterpart of AttemptWake's internal Nack calls.
func (s *Store) NackMessageDelivery(ctx context.Context, lease delivery.LeaseRef, retryable bool, errMsg string, nextAttemptIn time.Duration) (delivery.RecipientDelivery, delivery.Attempt, error) {
	var nextAttemptAt time.Time
	if nextAttemptIn > 0 {
		nextAttemptAt = time.Now().Add(nextAttemptIn)
	}
	return s.DeliveryStore().Nack(ctx, delivery.NackRequest{
		Lease:         lease,
		Retryable:     retryable,
		Error:         errMsg,
		NextAttemptAt: nextAttemptAt,
	})
}
