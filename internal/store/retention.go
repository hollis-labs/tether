package store

// retention.go — T09 (messaging vNext, CW-20260906-0040): the explicit,
// manual-only retention/purge mechanism the architecture calls for:
// "Define retention/expiry and safe migration controls; preserve bodies
// according to explicit policy." Before this file, nothing in the
// codebase purged message content -- confirmed by T09 design research:
// no scheduler, cron, or background sweep anywhere in internal/app or
// internal/store references retention/expiry/purge.
//
// Scope: this purges the CONTENT columns of one `messages` row (payload,
// metadata) -- never the structural/trace columns (id, kind, from_urn,
// to_urn, thread_id, created_at, delivery_id) a trace still needs to
// answer "who sent to whom," and never anything in go-messaging's own
// delivery-core tables. Reaching into that library's private schema with
// raw SQL to hard-delete delivery/attempt/receipt rows was already
// considered and rejected elsewhere in this package (see
// delivery_store.go's Send() doc comment: "adopt the library rather than
// hand-roll a parallel implementation") -- and the go-messaging
// delivery.Store interface exposes no delete primitive at all (confirmed
// against the vendored library source). Retention therefore stays inside
// the one table Tether fully owns.
//
// "Pending obligation" (T09 acceptance #3) is decided the same way
// internal/api/repair.go's redrive endpoint already decides "terminal":
// ONLY DeliveryDelivered and DeliveryCanceled count as done (mirrored
// here as isPurgeEligibleStatus, since store cannot import the api
// package's unexported isTerminalDeliveryStatus). DeliveryDeadLettered is
// deliberately NOT eligible even though it will never auto-retry — an
// operator can still redrive it (repair.go), and purging its body first
// would make that redrive resend an empty message, a real correctness
// bug this design avoids by construction. A pre-T03 legacy row (no
// delivery-core tracking at all, delivery_id NULL) is eligible only if
// its own consumed_at or canceled_at column is already set -- the
// pre-T03 definition of "done" -- otherwise it is conservatively treated
// as still-pending: unknown status never purges.
//
// No automatic sweep calls PurgeMessageBody or ListRetentionCandidates
// anywhere in this codebase -- both are invoked only by an explicit
// operator action (CLI or HTTP; see internal/api/retention.go), matching
// this sprint's manual-only launch policy and acceptance #3's "no silent
// deletion."
//
// Group-fanout messages (v060-05 T-04) are, by construction, never
// purge-eligible. A group's canonical `messages` row records the fanout
// under delivery_message_id (migration 0021), never delivery_id
// (migration 0020, 1:1-only) -- see registry/group_fanout.go. This code
// only ever inspects delivery_id, so a group row always falls into the
// legacyRowIsDone(consumed_at, canceled_at) branch below, and group
// reads are non-destructive (driven by group_members.last_read_seq, per
// GroupMessage's own doc comment in registry/model.go) -- consumed_at is
// never set for one. Net effect: PurgeMessageBody permanently refuses
// every group message with ErrPendingObligation, regardless of whether
// every recipient has read it. This is safe (retention always errs
// toward "never delete" over "delete something still needed") but is a
// deliberate, documented scope boundary, not an oversight -- extending
// purge eligibility to group fanout would need its own per-member
// completion signal this schema doesn't carry today.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
)

// ErrPendingObligation is returned by PurgeMessageBody when the message's
// delivery has not reached a purge-eligible terminal state (see this
// file's doc comment for exactly which statuses qualify). Nothing is
// mutated when this error is returned.
var ErrPendingObligation = errors.New("store: retention: message has a pending delivery obligation")

// RetentionCandidate is one row from ListRetentionCandidates: a message
// old enough to be considered, annotated with the delivery status an
// operator needs to decide whether purging it is safe right now.
type RetentionCandidate struct {
	MessageID   string
	CreatedAt   time.Time
	HasDelivery bool
	// Status is the delivery-core status as a string ("" when HasDelivery
	// is false, or when the delivery row itself could not be resolved).
	Status string
	// Eligible mirrors exactly what PurgeMessageBody would decide if
	// called right now for this message.
	Eligible bool
}

// rawRetentionRow is one unprocessed `messages` row, before classifying
// eligibility against the delivery core.
type rawRetentionRow struct {
	id, createdStr                  string
	consumedStr, canceledStr, delID sql.NullString
}

// queryRetentionRows runs the candidate-selection query and fully drains
// it into a slice before returning. Callers MUST NOT hold a *sql.Rows
// open across a call to GetDelivery (or any other query) -- s.db is
// configured with MaxOpenConns=1 (internal/store/sqlite.go), so an open
// outer Rows and a nested query would both wait on the single available
// connection forever. This is the same class of hazard
// delivery_store.go's Send() doc comment already documents for this
// package; splitting the query into its own function that closes its
// *sql.Rows via defer before returning is what makes ListRetentionCandidates
// safe to then loop over calling GetDelivery per row.
func (s *Store) queryRetentionRows(ctx context.Context, olderThan time.Time) ([]rawRetentionRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, consumed_at, canceled_at, delivery_id
		   FROM messages
		  WHERE created_at < ?
		  ORDER BY created_at ASC`,
		olderThan.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("store: retention: list candidates: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []rawRetentionRow
	for rows.Next() {
		var r rawRetentionRow
		if err := rows.Scan(&r.id, &r.createdStr, &r.consumedStr, &r.canceledStr, &r.delID); err != nil {
			return nil, fmt.Errorf("store: retention: scan candidate: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: retention: iterate candidates: %w", err)
	}
	return out, nil
}

// ListRetentionCandidates returns every message created before olderThan,
// oldest first, annotated with its current purge eligibility. Read-only:
// this is the explicit preview step an operator (or the CLI's
// `retention candidates` command) uses before calling PurgeMessageBody
// one message at a time -- nothing here mutates state.
func (s *Store) ListRetentionCandidates(ctx context.Context, olderThan time.Time) ([]RetentionCandidate, error) {
	raw, err := s.queryRetentionRows(ctx, olderThan)
	if err != nil {
		return nil, err
	}

	out := make([]RetentionCandidate, 0, len(raw))
	for _, r := range raw {
		created, perr := time.Parse(time.RFC3339Nano, r.createdStr)
		if perr != nil {
			created, _ = time.Parse(time.RFC3339, r.createdStr)
		}
		c := RetentionCandidate{MessageID: r.id, CreatedAt: created}

		if r.delID.Valid && r.delID.String != "" {
			c.HasDelivery = true
			if rd, getErr := s.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(r.delID.String)); getErr == nil {
				c.Status = string(rd.Status)
				c.Eligible = isPurgeEligibleStatus(rd.Status)
			}
		} else {
			c.Eligible = legacyRowIsDone(r.consumedStr, r.canceledStr)
		}
		out = append(out, c)
	}
	return out, nil
}

// PurgeMessageBody clears one message's content columns (payload,
// metadata) while leaving every structural/trace column intact -- see
// the file doc comment for exactly what "purge-eligible" means. Refuses
// with ErrPendingObligation, mutating nothing, when the message's
// delivery has not reached a purge-eligible terminal state. Idempotent:
// purging an already-purged (or never-had-a-body) message returns
// purged=false with no error, rather than erroring, matching this
// sprint's established idempotent-repair convention (repair.go's
// redrive).
func (s *Store) PurgeMessageBody(ctx context.Context, messageID string) (purged bool, err error) {
	var payload, metadata, consumedStr, canceledStr, delID sql.NullString
	row := s.db.QueryRowContext(ctx,
		`SELECT payload, metadata, consumed_at, canceled_at, delivery_id FROM messages WHERE id = ?`,
		messageID,
	)
	if scanErr := row.Scan(&payload, &metadata, &consumedStr, &canceledStr, &delID); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return false, fmt.Errorf("store: retention: purge %s: %w", messageID, messaging.ErrNotFound)
		}
		return false, fmt.Errorf("store: retention: purge %s: %w", messageID, scanErr)
	}

	eligible := false
	if delID.Valid && delID.String != "" {
		rd, getErr := s.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(delID.String))
		if getErr != nil {
			return false, fmt.Errorf("store: retention: purge %s: get delivery: %w", messageID, getErr)
		}
		eligible = isPurgeEligibleStatus(rd.Status)
	} else {
		eligible = legacyRowIsDone(consumedStr, canceledStr)
	}
	if !eligible {
		return false, ErrPendingObligation
	}

	if !payload.Valid && !metadata.Valid {
		// Already purged (or the message never had a body) — idempotent no-op.
		return false, nil
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE messages SET payload = NULL, metadata = NULL WHERE id = ?`, messageID,
	); err != nil {
		return false, fmt.Errorf("store: retention: purge %s: %w", messageID, err)
	}
	return true, nil
}

// isPurgeEligibleStatus mirrors internal/api/repair.go's
// isTerminalDeliveryStatus exactly (Delivered + Canceled only) --
// deliberately excluding DeliveryDeadLettered, which remains repairable
// via redrive (see file doc comment). Kept as a separate copy rather than
// an import: store must not depend on api (api already depends on
// store), and this is a two-case predicate, not a shared abstraction
// worth inverting the dependency for.
func isPurgeEligibleStatus(st delivery.DeliveryStatus) bool {
	switch st {
	case delivery.DeliveryDelivered, delivery.DeliveryCanceled:
		return true
	default:
		return false
	}
}

// legacyRowIsDone applies the pre-T03 definition of "done" to a message
// with no delivery-core tracking: either lifecycle column being set is
// sufficient, matching how the old delivered_at/consumed_at state
// machine (superseded by T03, see delivery_store.go) considered a
// message finished.
func legacyRowIsDone(consumedStr, canceledStr sql.NullString) bool {
	return (consumedStr.Valid && consumedStr.String != "") || (canceledStr.Valid && canceledStr.String != "")
}
