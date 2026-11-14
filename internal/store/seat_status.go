package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/hollis-labs/tether/internal/seatstatus"
)

// GetSeatStatus returns the agent's declared status; found is false when it has
// never declared one.
func (s *Store) GetSeatStatus(ctx context.Context, agentURN string) (seatstatus.Declared, bool, error) {
	var d seatstatus.Declared
	var state, since, updated string
	var expires sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT agent_urn,state,reason,ref,owner,since,expires_at,set_by,updated_at FROM seat_status WHERE agent_urn=?`, agentURN).
		Scan(&d.AgentURN, &state, &d.Reason, &d.Ref, &d.Owner, &since, &expires, &d.SetBy, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	d.State = seatstatus.State(state)
	d.Since, _ = time.Parse(time.RFC3339Nano, since)
	d.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if expires.Valid {
		d.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires.String)
	}
	return d, true, nil
}

// PutSeatStatus stores d and reports whether it changed what the seat
// declares (state, reason, ref or owner). An unchanged re-set keeps the
// original since and only refreshes expiry, set_by and updated_at.
func (s *Store) PutSeatStatus(ctx context.Context, d seatstatus.Declared) (seatstatus.Declared, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var prevState, prevReason, prevRef, prevOwner, prevSince string
	err = tx.QueryRowContext(ctx, `SELECT state,reason,ref,owner,since FROM seat_status WHERE agent_urn=?`, d.AgentURN).Scan(&prevState, &prevReason, &prevRef, &prevOwner, &prevSince)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, false, err
	}
	prev := seatstatus.Declared{State: seatstatus.State(prevState), Reason: prevReason, Ref: prevRef, Owner: prevOwner}
	changed := !found || !prev.SameAs(d)
	if !changed {
		d.Since, _ = time.Parse(time.RFC3339Nano, prevSince)
	}
	var expires any
	if !d.ExpiresAt.IsZero() {
		expires = d.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO seat_status(agent_urn,state,reason,ref,owner,since,expires_at,set_by,updated_at) VALUES (?,?,?,?,?,?,?,?,?)
 ON CONFLICT(agent_urn) DO UPDATE SET state=excluded.state,reason=excluded.reason,ref=excluded.ref,owner=excluded.owner,since=excluded.since,expires_at=excluded.expires_at,set_by=excluded.set_by,updated_at=excluded.updated_at`,
		d.AgentURN, string(d.State), d.Reason, d.Ref, d.Owner, d.Since.UTC().Format(time.RFC3339Nano), expires, d.SetBy, d.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return d, false, err
	}
	return d, changed, tx.Commit()
}

// SessionLastEventAt is the time of the session's newest event (MAX(at) over
// idx_events_session), zero when it has none.
func (s *Store) SessionLastEventAt(ctx context.Context, sessionID string) (time.Time, error) {
	var at sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(at) FROM events WHERE session_id=?`, sessionID).Scan(&at); err != nil {
		return time.Time{}, err
	}
	if !at.Valid || at.String == "" {
		return time.Time{}, nil
	}
	// An unparseable stamp reads as no activity rather than failing the card.
	t, _ := time.Parse(time.RFC3339Nano, at.String)
	return t, nil
}
