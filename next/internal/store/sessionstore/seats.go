package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// SeatStatusRow is a seat's declared status (seatstatus.Declared in the old tree; validating it is
// the seat slice's). A zero ExpiresAt is stored NULL.
type SeatStatusRow struct {
	AgentURN, State, Reason, Ref, Owner, SetBy string
	Since, ExpiresAt, UpdatedAt                time.Time
}

// wsSameDeclaration is seatstatus.Declared.SameAs: what the seat declares, not when or by whom.
func wsSameDeclaration(a, b SeatStatusRow) bool {
	return a.State == b.State && a.Reason == b.Reason && a.Ref == b.Ref && a.Owner == b.Owner
}

// GetSeatStatus returns the agent's declared status; found is false when it never declared one.
// An unparsable stored time reads as zero, as before.
func (s *Store) GetSeatStatus(ctx context.Context, agentURN string) (SeatStatusRow, bool, error) {
	r := s.c.Reader()
	var d SeatStatusRow
	var since, updated string
	var expires sql.NullString
	err := r.QueryRowContext(ctx, rebind(r, `SELECT agent_urn, state, reason, ref, owner, since, expires_at, set_by, updated_at FROM seat_status WHERE agent_urn = ?`), agentURN).
		Scan(&d.AgentURN, &d.State, &d.Reason, &d.Ref, &d.Owner, &since, &expires, &d.SetBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SeatStatusRow{}, false, nil
	}
	if err != nil {
		return SeatStatusRow{}, false, err
	}
	d.Since, _ = time.Parse(time.RFC3339Nano, since)
	d.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if expires.Valid {
		d.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires.String)
	}
	return d, true, nil
}

// PutSeatStatus stores d and reports whether it changed what the seat declares (state, reason, ref
// or owner). An unchanged re-set keeps the stored since and only refreshes expiry, set_by and
// updated_at. The read of the previous declaration and the write run under a per-seat lock (on
// Postgres a transaction advisory lock on the agent URN, held before the row may exist; on SQLite
// the one writer), so of two concurrent identical declarations exactly one reports changed.
func (s *Store) PutSeatStatus(ctx context.Context, d SeatStatusRow) (SeatStatusRow, bool, error) {
	var out SeatStatusRow
	var changed bool
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		out, changed = d, false
		if err := wsLockSeat(ctx, tx, d.AgentURN); err != nil {
			return err
		}
		var prev SeatStatusRow
		var prevSince string
		err := tx.QueryRowContext(ctx, rebind(tx, `SELECT state, reason, ref, owner, since FROM seat_status WHERE agent_urn = ?`), d.AgentURN).
			Scan(&prev.State, &prev.Reason, &prev.Ref, &prev.Owner, &prevSince)
		found := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		changed = !found || !wsSameDeclaration(prev, d)
		if !changed {
			out.Since, _ = time.Parse(time.RFC3339Nano, prevSince)
		}
		var expires any
		if !out.ExpiresAt.IsZero() {
			expires = out.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		_, err = tx.ExecContext(ctx, rebind(tx, `INSERT INTO seat_status (agent_urn, state, reason, ref, owner, since, expires_at, set_by, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (agent_urn) DO UPDATE SET state = excluded.state, reason = excluded.reason, ref = excluded.ref, owner = excluded.owner,
 since = excluded.since, expires_at = excluded.expires_at, set_by = excluded.set_by, updated_at = excluded.updated_at`),
			out.AgentURN, out.State, out.Reason, out.Ref, out.Owner, out.Since.UTC().Format(time.RFC3339Nano), expires, out.SetBy,
			out.UpdatedAt.UTC().Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		return d, false, err
	}
	return out, changed, nil
}

// wsLockSeat serializes PutSeatStatus per seat on Postgres (READ COMMITTED, concurrent writers): a
// row lock cannot cover a seat that has no row yet, so it is a transaction advisory lock on the URN.
func wsLockSeat(ctx context.Context, tx store.Tx, agentURN string) error {
	if tx.Dialect().Name() != gostore.DriverPostgres {
		return nil
	}
	rows, err := tx.QueryContext(ctx, rebind(tx, `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`), "seat_status:"+agentURN)
	if err != nil {
		return err
	}
	return rows.Close()
}
