package store

// delivery_legacy_import.go — T03 (messaging vNext, CW-20260904-0100),
// acceptance #2: "Fixture migration verifies message/body/thread integrity
// and independent attention state; ambiguous old delivered rows follow
// reviewed policy with no silent data loss."
//
// go-messaging's own delivery.MigrateLegacyMailbox is hardcoded to a
// specific legacy table (`agent_messages`, session/agent-tuple addressing)
// that does not match Tether's actual `messages` schema (single from_urn/
// to_urn columns) -- confirmed by reading its source. This file is Tether's
// own importer, following the SAME policy shape go-messaging's own
// compatibility guide (libs/go-messaging/docs/messaging-vnext-compatibility.md)
// prescribes, rather than a literal call to that function.
//
// This is NOT wired into Store.Open or any other automatic startup path.
// It is an explicit, operator-invoked, one-time backfill for rows that
// predate T03 (delivery_id IS NULL) -- running it is a deliberate decision
// on a specific database, exercised here only against fixture copies, never
// live ~/.tether data, per the execution contract's explicit instruction.
//
// Per-row policy, based on Tether's OWN schema (which disambiguates more
// precisely than go-messaging's legacy agent_messages table does -- see
// the T01 contract §2.4.1/§3.2 and the reasoning below):
//
//   - canceled_at IS NOT NULL: skipped. The sender explicitly aborted the
//     message before any delivery obligation should exist; creating one now
//     would be manufacturing an obligation nobody asked for.
//   - group_urn IS NOT NULL: skipped. Group message fanout reconciliation
//     is T04's explicit scope (durable per-recipient receipts on top of the
//     shared room body), not a T03 concern -- importing a group post as an
//     ordinary 1:1 delivery here would misrepresent its fanout semantics.
//   - delivered_at IS NULL: UNAMBIGUOUS -- Inbox never pulled this row, so
//     it was never delivered under the old model either. Imported as a
//     live PENDING delivery obligation (ImportedPending) -- this is not the
//     ambiguous case at all, since Tether's own delivered_at column already
//     answers the question go-messaging's legacy status column can't.
//   - delivered_at IS NOT NULL AND consumed_at IS NULL: the genuinely
//     AMBIGUOUS case (T01 §2.4.1's crash-recovery gap -- a consumer may have
//     received it and simply never called Consume, or crashed first). Held
//     as dead-lettered pending authorized redrive under the default policy
//     (ImportHoldAmbiguousDelivered), matching go-messaging's own default
//     (LegacyMailboxHoldAmbiguousUnread) -- no blind replay. The explicit
//     opt-in ImportReplayAmbiguousDelivered instead imports these as
//     pending, to be redelivered.
//   - delivered_at IS NOT NULL AND consumed_at IS NOT NULL: confirmed
//     complete. Imported as completed history (host_accepted/turn_submitted
//     backdated to delivered_at, consumed backdated to consumed_at) --
//     no fabricated receipts beyond what already happened, and none
//     invented that didn't (e.g. no fake lease/attempt timing beyond the
//     two real timestamps that exist).
//
// Message content integrity: messages.id, thread_id, in_reply_to and all
// other columns are NEVER modified by this importer -- only the new
// messages.delivery_id column is populated. The delivery core's own
// internal Message.ID for an imported row is necessarily a freshly minted
// value distinct from messages.id (Enqueue always mints its own ID; there
// is no way to pin it, confirmed against the library's public API) -- this
// is fine because messages.id remains, as always, the sole canonical
// content identifier for every existing caller (Get/Thread/List all read
// from `messages`, never from the delivery core's Message.ID). Only
// messages.delivery_id needs to be a valid handle into the delivery core,
// and it is.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
)

// LegacyImportPolicy controls how ambiguous (delivered-but-unconsumed) rows
// are imported. See the file-level doc comment for the full per-row policy.
type LegacyImportPolicy string

const (
	// ImportHoldAmbiguousDelivered is the safe default: rows with
	// delivered_at set but consumed_at unset import as dead-lettered,
	// pending authorized redrive.
	ImportHoldAmbiguousDelivered LegacyImportPolicy = "hold_ambiguous_delivered"
	// ImportReplayAmbiguousDelivered is an explicit opt-in: the same rows
	// import as pending (will be redelivered) instead of dead-lettered.
	ImportReplayAmbiguousDelivered LegacyImportPolicy = "replay_ambiguous_delivered"
)

// LegacyImportResult summarizes one ImportLegacyMessagesIntoDelivery run.
type LegacyImportResult struct {
	Imported         int // rows that got a new delivery_id mapping
	SkippedCanceled  int
	SkippedGroup     int
	SkippedAlready   int // delivery_id already set -- not re-imported
	ImportedPending  int // delivered_at IS NULL
	ImportedHeld     int // ambiguous, held per policy
	ImportedReplay   int // ambiguous, replayed per policy
	ImportedComplete int // delivered_at + consumed_at both set
}

// ImportLegacyMessagesIntoDelivery is the one-time backfill: for every
// `messages` row with delivery_id IS NULL (excluding canceled and group
// rows, see file doc), it creates the corresponding delivery-core
// obligation per the policy above and records the mapping. Idempotent by
// construction (only rows with delivery_id IS NULL are considered, so a
// second run does nothing to already-imported rows -- reflected in
// SkippedAlready).
//
// db is the raw *sql.DB (used to read/update `messages` rows directly);
// deliveryStore is typically (*Store).DeliveryStore() for the same db.
// Callers MUST pass a fixture/copy database, never live ~/.tether state --
// this function performs real writes with no dry-run mode.
func ImportLegacyMessagesIntoDelivery(ctx context.Context, db *sql.DB, deliveryStore delivery.Store, policy LegacyImportPolicy) (LegacyImportResult, error) {
	if policy == "" {
		policy = ImportHoldAmbiguousDelivered
	}
	var result LegacyImportResult

	rows, err := db.QueryContext(ctx, `
		SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
		       payload, content_type, metadata, created_at, delivered_at,
		       consumed_at, canceled_at, group_urn
		FROM messages
		WHERE delivery_id IS NULL
		ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return result, fmt.Errorf("legacy import: query: %w", err)
	}
	type legacyRow struct {
		id, kind, fromURN, toURN                                     string
		channel, threadID, inReplyTo, payload, contentType, metadata sql.NullString
		createdAt                                                    string
		deliveredAt, consumedAt, canceledAt, groupURN                sql.NullString
	}
	var pending []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.id, &r.kind, &r.channel, &r.fromURN, &r.toURN,
			&r.threadID, &r.inReplyTo, &r.payload, &r.contentType, &r.metadata,
			&r.createdAt, &r.deliveredAt, &r.consumedAt, &r.canceledAt, &r.groupURN); err != nil {
			rows.Close()
			return result, fmt.Errorf("legacy import: scan: %w", err)
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("legacy import: rows: %w", err)
	}
	rows.Close()

	for _, r := range pending {
		if r.canceledAt.Valid {
			result.SkippedCanceled++
			continue
		}
		if r.groupURN.Valid {
			result.SkippedGroup++
			continue
		}

		from, err := messaging.ParseURN(r.fromURN)
		if err != nil {
			return result, fmt.Errorf("legacy import: row %s: parse from_urn %q: %w", r.id, r.fromURN, err)
		}
		to, err := messaging.ParseURN(r.toURN)
		if err != nil {
			return result, fmt.Errorf("legacy import: row %s: parse to_urn %q: %w", r.id, r.toURN, err)
		}
		if _, err := time.Parse(time.RFC3339Nano, r.createdAt); err != nil {
			return result, fmt.Errorf("legacy import: row %s: parse created_at %q: %w", r.id, r.createdAt, err)
		}

		var deliveredAt, consumedAt time.Time
		if r.deliveredAt.Valid {
			deliveredAt, err = time.Parse(time.RFC3339Nano, r.deliveredAt.String)
			if err != nil {
				return result, fmt.Errorf("legacy import: row %s: parse delivered_at %q: %w", r.id, r.deliveredAt.String, err)
			}
		}
		if r.consumedAt.Valid {
			consumedAt, err = time.Parse(time.RFC3339Nano, r.consumedAt.String)
			if err != nil {
				return result, fmt.Errorf("legacy import: row %s: parse consumed_at %q: %w", r.id, r.consumedAt.String, err)
			}
		}

		held := r.deliveredAt.Valid && !r.consumedAt.Valid && policy != ImportReplayAmbiguousDelivered
		enqueueRes, err := deliveryStore.Enqueue(ctx, delivery.EnqueueRequest{
			From:        from,
			Recipients:  []delivery.RecipientTarget{{Address: to}},
			Kind:        messaging.Kind(r.kind),
			Channel:     messaging.Channel(r.channel.String),
			ThreadID:    r.threadID.String,
			InReplyTo:   r.inReplyTo.String,
			Payload:     []byte(r.payload.String),
			ContentType: r.contentType.String,
			Metadata:    map[string]string{"legacy_message_id": r.id, "legacy_created_at": r.createdAt},
		})
		if err != nil {
			return result, fmt.Errorf("legacy import: row %s: enqueue: %w", r.id, err)
		}
		deliveryID := enqueueRes.Deliveries[0].ID

		switch {
		case !r.deliveredAt.Valid:
			// Unambiguous: never delivered. Leave pending as-is.
			result.ImportedPending++

		case !r.consumedAt.Valid:
			// Ambiguous: delivered but never confirmed consumed.
			if !held {
				result.ImportedReplay++
				break
			}
			if err := holdAsDeadLettered(ctx, deliveryStore, deliveryID, deliveredAt); err != nil {
				return result, fmt.Errorf("legacy import: row %s: hold ambiguous: %w", r.id, err)
			}
			result.ImportedHeld++

		default:
			// Confirmed complete: replay the real historical timeline.
			if err := replayCompletedHistory(ctx, deliveryStore, deliveryID, deliveredAt, consumedAt); err != nil {
				return result, fmt.Errorf("legacy import: row %s: replay completed history: %w", r.id, err)
			}
			result.ImportedComplete++
		}

		if _, err := db.ExecContext(ctx, `UPDATE messages SET delivery_id=? WHERE id=?`, string(deliveryID), r.id); err != nil {
			return result, fmt.Errorf("legacy import: row %s: record delivery_id: %w", r.id, err)
		}
		result.Imported++
	}

	var already int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE delivery_id IS NOT NULL`).Scan(&already); err == nil {
		result.SkippedAlready = already - result.Imported
	}
	return result, nil
}

func holdAsDeadLettered(ctx context.Context, store delivery.Store, deliveryID delivery.DeliveryID, at time.Time) error {
	claim, err := store.Claim(ctx, delivery.ClaimRequest{DeliveryID: deliveryID, Holder: "legacy-import", LeaseDuration: time.Minute})
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	// Nack's own logic (delivery/sqlite.go, confirmed by reading it directly)
	// dead-letters unconditionally when Retryable is false, independent of
	// any deadline -- no DeadlineAt gymnastics needed. Backdated to `at`
	// (the row's real historical delivered_at) so the receipt trail reflects
	// when the ambiguity actually arose, not the import run's own clock.
	_, _, err = store.Nack(ctx, delivery.NackRequest{
		Lease:     lease,
		Retryable: false,
		Error:     "legacy mailbox delivery state ambiguous; authorize redrive to replay",
		At:        at,
	})
	// Nack returns ErrDeadLettered as the EXPECTED signal that the terminal
	// transition happened (confirmed by reading delivery/sqlite.go directly)
	// -- not a failure of this operation.
	if err != nil && !errors.Is(err, delivery.ErrDeadLettered) {
		return fmt.Errorf("nack: %w", err)
	}
	del, err := store.GetDelivery(ctx, deliveryID)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if del.Status != delivery.DeliveryDeadLettered {
		return fmt.Errorf("expected dead_lettered after ambiguous-hold nack (Retryable:false), got %q", del.Status)
	}
	return nil
}

func replayCompletedHistory(ctx context.Context, store delivery.Store, deliveryID delivery.DeliveryID, deliveredAt, consumedAt time.Time) error {
	claim, err := store.Claim(ctx, delivery.ClaimRequest{DeliveryID: deliveryID, Holder: "legacy-import", LeaseDuration: time.Hour})
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	for _, step := range []struct {
		stage delivery.ReceiptStage
		at    time.Time
	}{
		{delivery.StageHostAccepted, deliveredAt},
		{delivery.StageTurnSubmitted, deliveredAt},
		{delivery.StageConsumed, consumedAt},
	} {
		if _, _, err := store.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: step.stage, At: step.at}); err != nil {
			return fmt.Errorf("ack %s: %w", step.stage, err)
		}
	}
	return nil
}
