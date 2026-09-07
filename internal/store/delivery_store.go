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
// methods (sendWithID, consumeAndReportDeliveryID) that exist solely to
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
// as before (delegated to the wrapped InboxStore), then claims and
// acknowledges the corresponding delivery through
// host_accepted/turn_submitted/consumed in one immediate sequence. Tether's
// Consume has no separate "I'm now processing" signal distinct from "I
// finished processing" (T02 design research), so all three stages are
// recorded together rather than held open as a real lease.
//
// Fixed for CW-20260907-0033: receipt recording is attempted on EVERY call
// that has a delivery_id, not only the call that transitions consumed_at
// from NULL to set. The two writes (messages.consumed_at, delivery-core
// receipts) are against separate stores with no shared transaction --
// consumed_at can commit and the process can then crash/fail before
// recording receipts, permanently stranding that delivery in a non-terminal
// state (never redriven, since delivery-core sees no ack; never purgeable,
// since it never reaches Delivered) with delivery-core's own retry/wake
// logic repeatedly re-attempting a message the recipient already consumed.
// Re-attempting on every idempotent replay (including a client's own
// retry-after-connection-loss, which Consume's documented idempotency
// already invites) makes this self-healing: recordConsumedReceipts is
// itself best-effort and safe to call against an already-terminal delivery
// (Claim fails cleanly, logged, no-op -- see below and
// TestDeliveryBackedConsume_RecordsReceiptsIdempotently's replay
// assertion), so retrying it costs nothing on the already-complete path and
// closes the crash window on the incomplete one.
//
// Delivery-core recording here remains best-effort: if the message predates
// T03 (no delivery_id recorded) or the delivery-core call fails for any
// reason (including a genuine race against another claimant), Consume still
// succeeds from the caller's perspective -- the `messages.consumed_at`
// update is Consume's core, tested contract; delivery-core receipt tracking
// is an additive enhancement layered on top, consistent with how this
// codebase already treats other enhancement-only writes (e.g.
// SetClaudeSessionID/UpsertSessionProviderMapping failures are logged, not
// propagated).
func (d *deliveryBackedStore) Consume(ctx context.Context, id string, recipient messaging.Address) error {
	deliveryID, err := d.consumeAndReportDeliveryID(ctx, id, recipient)
	if err != nil {
		return err
	}
	if deliveryID == "" {
		return nil
	}
	d.recordConsumedReceipts(ctx, id, deliveryID, recipient)
	return nil
}

func (d *deliveryBackedStore) recordConsumedReceipts(ctx context.Context, messageID, deliveryID string, recipient messaging.Address) {
	lease, err := d.leaseForConsumedReceipts(ctx, messageID, deliveryID, recipient)
	if err != nil {
		log.Printf("delivery store: consume: could not obtain a lease to record receipts for delivery %s (best-effort receipt recording skipped): %v", deliveryID, err)
		return
	}
	for _, stage := range []delivery.ReceiptStage{delivery.StageHostAccepted, delivery.StageTurnSubmitted, delivery.StageConsumed} {
		if _, _, err := d.delivery.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: stage}); err != nil {
			log.Printf("delivery store: consume: ack stage %s for delivery %s failed (best-effort): %v", stage, deliveryID, err)
			return
		}
	}
}

// leaseForConsumedReceipts returns a LeaseRef usable to Ack deliveryID
// through to Consumed.
//
// Fixed for a remaining instance of CW-20260907-0033 (found by independent
// review after the first fix landed): the delivery most commonly already
// has a LIVE, unexpired lease at Consume time -- Tether's own wake pump
// (internal/app/wake.go's attemptWake) claims the delivery and Acks it up
// through StageTurnSubmitted, then deliberately leaves the lease open
// "awaiting consumption" (Consume is what's supposed to close it out).
// Attempting a brand-new Claim in that state always fails with
// ErrAlreadyClaimed -- go-messaging correctly refuses ANY new claim while a
// lease is active -- and the old code logged and discarded that error,
// leaving the delivery stuck at whatever stage the wake left it,
// non-terminal, until the lease eventually expired and delivery-core's own
// retry/wake logic re-attempted delivery of content the recipient had
// already consumed (a duplicate wake, reproduced with no crash required).
//
// The fix reuses that active lease -- but ONLY when migration 0022's
// pending_receipt_attempt_id/pending_receipt_lease_token marker on the
// `messages` row exactly matches the delivery's CURRENT active attempt.
// That marker is written by attemptWake right after it claims (not just
// after turn_submitted) and by this function's own fallback Claim branch
// below right after IT claims -- i.e. only by code that intends to hand
// the lease to Consume. A second independent reviewer pass (after this
// function's first version blindly reused ANY active lease) found this
// was NOT safe as originally written: T07's published-local bridge surface
// (POST /messages/{id}/claim|ack|nack, internal/api/messages.go) lets a
// completely independent external process durably claim and drive the
// SAME delivery at its own pace, and its handleMessageClaim defaults its
// `holder` to the very same asserted recipient URN Consume itself uses --
// so Holder identity cannot distinguish "my own wake/Consume attempt" from
// "an independent bridge's still-in-flight attempt." Blindly reusing the
// bridge's lease could finish (or a later legitimate Nack from the bridge
// could downgrade) work the bridge had not actually completed. The marker
// is the unambiguous signal instead: the bridge path never writes it, so
// its leases are correctly left alone here and this function falls back to
// attempting a fresh Claim, which fails safely (logged, discarded) exactly
// as it did before this fix, for exactly the case where that's correct.
//
// go-messaging's Ack fencing (currentLease) checks only
// AttemptID/LeaseToken/BindingGeneration, never Holder identity, so once
// the marker confirms this is a Consume-owned attempt, completing it is
// exactly the correct operation -- not a race against a different
// claimant, just this delivery's one real in-flight attempt finishing.
// This is also what makes the four originally-reported cases converge on
// one fix: restarting after lease_acquired/host_accepted/turn_submitted
// all reuse that same still-valid, still-marked lease and complete it
// immediately, and completing it (rather than leaving it to expire) is
// what stops the wake sweep from ever seeing the delivery as retry-eligible
// again.
//
// Falls back to a fresh Claim when there is no active lease, or no
// matching marker, to reuse (a purely client-initiated Consume that no
// wake ever touched, or a delivery an independent claimant currently
// holds), matching the pre-fix behavior for those cases. A lease that
// expires in the narrow window between this read and the Ack calls below
// still fails safely (Ack itself re-validates expiry and returns
// ErrStaleLease, handled by the existing best-effort logging in the
// caller) -- an accepted residual, orders of magnitude narrower than the
// deterministic bug this replaces. Note this residual is NOT automatically
// retried the way the crash-before-first-Ack window is: Consume already
// returned success to its own caller by this point (recordConsumedReceipts
// runs after that response is decided), so nothing here forces a future
// Consume call to happen on its own -- correctness in this narrow window
// still depends on some later idempotent replay occurring (a client retry,
// a redrive, another notification), not a guarantee this code provides by
// itself.
func (d *deliveryBackedStore) leaseForConsumedReceipts(ctx context.Context, messageID, deliveryID string, recipient messaging.Address) (delivery.LeaseRef, error) {
	if del, err := d.delivery.GetDelivery(ctx, delivery.DeliveryID(deliveryID)); err == nil &&
		del.Status == delivery.DeliveryLeased && del.ActiveAttemptID != "" && del.ActiveLeaseToken != "" {
		if pendingAttemptID, pendingLeaseToken, err := pendingReceiptLease(ctx, d.db, messageID); err == nil &&
			pendingAttemptID != "" && pendingLeaseToken != "" &&
			delivery.AttemptID(pendingAttemptID) == del.ActiveAttemptID &&
			delivery.LeaseToken(pendingLeaseToken) == del.ActiveLeaseToken {
			if attempts, err := d.delivery.Attempts(ctx, delivery.DeliveryID(deliveryID)); err == nil {
				for _, a := range attempts {
					if a.ID == del.ActiveAttemptID && a.LeaseToken == del.ActiveLeaseToken {
						return delivery.LeaseRef{
							DeliveryID:        del.ID,
							AttemptID:         a.ID,
							LeaseToken:        a.LeaseToken,
							BindingGeneration: a.BindingGeneration,
						}, nil
					}
				}
			}
		}
	}
	claim, err := d.delivery.Claim(ctx, delivery.ClaimRequest{
		DeliveryID:    delivery.DeliveryID(deliveryID),
		Holder:        recipient.URN(),
		LeaseDuration: deliveryConsumeLeaseDuration,
		Nowait:        true,
	})
	if err != nil {
		return delivery.LeaseRef{}, err
	}
	// Mark this freshly-claimed attempt as Consume-owned so a crash before
	// finishing the Ack sequence below is still recoverable on a later
	// retry (see the doc comment above).
	if err := setPendingReceiptLease(ctx, d.db, messageID, claim.Attempt.ID, claim.Attempt.LeaseToken); err != nil {
		log.Printf("delivery store: consume: recording pending-receipt marker for message %s failed (best-effort; a crash before finishing this Ack sequence would not be recoverable on retry): %v", messageID, err)
	}
	return delivery.LeaseRef{
		DeliveryID:        claim.Attempt.DeliveryID,
		AttemptID:         claim.Attempt.ID,
		LeaseToken:        claim.Attempt.LeaseToken,
		BindingGeneration: claim.Attempt.BindingGeneration,
	}, nil
}

// setPendingReceiptLease and pendingReceiptLease read/write migration
// 0022's marker columns -- see leaseForConsumedReceipts's doc comment for
// why they exist. Package-level (not methods) so both deliveryBackedStore
// (via its embedded *messagingStore's db field) and *Store (for
// internal/app's attemptWake, which has no messagingStore of its own) can
// call the same logic against their respective *sql.DB.
func setPendingReceiptLease(ctx context.Context, db *sql.DB, messageID string, attemptID delivery.AttemptID, leaseToken delivery.LeaseToken) error {
	_, err := db.ExecContext(ctx,
		`UPDATE messages SET pending_receipt_attempt_id=?, pending_receipt_lease_token=? WHERE id=?`,
		string(attemptID), string(leaseToken), messageID)
	return err
}

func pendingReceiptLease(ctx context.Context, db *sql.DB, messageID string) (attemptID, leaseToken string, err error) {
	var a, l sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT pending_receipt_attempt_id, pending_receipt_lease_token FROM messages WHERE id=?`, messageID,
	).Scan(&a, &l); err != nil {
		return "", "", err
	}
	return a.String, l.String, nil
}

// SetPendingReceiptLease exposes setPendingReceiptLease to callers outside
// this package's own Consume path -- specifically internal/app's
// attemptWake, which must mark its own claim as Consume-owned immediately
// after claiming (see leaseForConsumedReceipts's doc comment).
func (s *Store) SetPendingReceiptLease(ctx context.Context, messageID string, attemptID delivery.AttemptID, leaseToken delivery.LeaseToken) error {
	return setPendingReceiptLease(ctx, s.db, messageID, attemptID, leaseToken)
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
