package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Wake triage storage (CW-20261114-0017).

// Triage states.
const (
	TriagePending  = "pending"  // classified, not yet acted on
	TriageWoken    = "woken"    // a wake turn was submitted for it
	TriageCovered  = "covered"  // another wake of the same seat announced it
	TriageSurfaced = "surfaced" // fyi: listed on a human-originated turn
	TriageSettled  = "settled"  // read: marked read by the hub
)

// TriageRow is one message's triage.
type TriageRow struct {
	MessageID string
	Recipient string
	Seat      string
	Class     string
	Reason    string
	Typed     bool
	State     string
	NotBefore time.Time
	CreatedAt time.Time
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// TriageGet returns the message's triage, found false when it has none.
func (s *Store) TriageGet(ctx context.Context, messageID string) (TriageRow, bool, error) {
	var r TriageRow
	var typed int
	var notBefore sql.NullString
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT message_id,recipient,seat,class,reason,typed,state,not_before,created_at FROM wake_triage WHERE message_id=?`, messageID).
		Scan(&r.MessageID, &r.Recipient, &r.Seat, &r.Class, &r.Reason, &typed, &r.State, &notBefore, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.Typed = typed == 1
	if notBefore.Valid {
		r.NotBefore, _ = time.Parse(time.RFC3339Nano, notBefore.String)
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return r, true, nil
}

// TriageRecord stores a new triage and reports whether this call created it
// (false: one already existed; the stored row is returned either way).
func (s *Store) TriageRecord(ctx context.Context, r TriageRow) (TriageRow, bool, error) {
	if r.State == "" {
		r.State = TriagePending
	}
	typed := 0
	if r.Typed {
		typed = 1
	}
	now := ts(r.CreatedAt)
	res, err := s.db.ExecContext(ctx, `INSERT INTO wake_triage(message_id,recipient,seat,class,reason,typed,state,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)
 ON CONFLICT(message_id) DO NOTHING`, r.MessageID, r.Recipient, r.Seat, r.Class, r.Reason, typed, r.State, now, now)
	if err != nil {
		return r, false, err
	}
	n, _ := res.RowsAffected()
	stored, _, err := s.TriageGet(ctx, r.MessageID)
	return stored, n == 1, err
}

// TriageSetState moves a message's triage to state.
func (s *Store) TriageSetState(ctx context.Context, messageID, state string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE wake_triage SET state=?, updated_at=? WHERE message_id=?`, state, ts(at), messageID)
	return err
}

// TriageDefer sets a pending action's earliest wake.
func (s *Store) TriageDefer(ctx context.Context, messageID string, notBefore, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE wake_triage SET not_before=?, updated_at=? WHERE message_id=? AND state='pending'`, ts(notBefore), ts(at), messageID)
	return err
}

// TriageCoverPending marks the seat's other pending action mail covered by a
// wake that just announced it (the wake text counts every unread message).
func (s *Store) TriageCoverPending(ctx context.Context, seat, exceptMessageID string, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE wake_triage SET state='covered', updated_at=? WHERE seat=? AND state='pending' AND class='action' AND message_id<>?`, ts(at), seat, exceptMessageID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SeatLastActionWake is the seat's last action wake, zero when none.
func (s *Store) SeatLastActionWake(ctx context.Context, seat string) (time.Time, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT last_action_wake_at FROM seat_wake_clock WHERE seat=?`, seat).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339Nano, raw)
	return t, nil
}

// SetSeatLastActionWake records an action wake of the seat.
func (s *Store) SetSeatLastActionWake(ctx context.Context, seat string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO seat_wake_clock(seat,last_action_wake_at) VALUES (?,?)
 ON CONFLICT(seat) DO UPDATE SET last_action_wake_at=excluded.last_action_wake_at`, seat, ts(at))
	return err
}

// FYIItem is an fyi message waiting to ride along on a human turn.
type FYIItem struct {
	MessageID string
	Recipient string
	From      string
	Typed     bool
	Subject   string
}

// TriageFYIPending lists the seat's pending fyi mail that is still unread,
// oldest first.
func (s *Store) TriageFYIPending(ctx context.Context, seat string, limit int) ([]FYIItem, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.message_id, t.recipient, m.from_urn, t.typed, coalesce(m.payload,'') FROM wake_triage t JOIN messages m ON m.id=t.message_id
 WHERE t.seat=? AND t.class='fyi' AND t.state='pending' AND m.read_at IS NULL AND m.canceled_at IS NULL AND m.archived_at IS NULL
 ORDER BY t.created_at LIMIT ?`, seat, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FYIItem
	for rows.Next() {
		var it FYIItem
		var typed int
		var payload string
		if err := rows.Scan(&it.MessageID, &it.Recipient, &it.From, &typed, &payload); err != nil {
			return nil, err
		}
		it.Typed = typed == 1
		it.Subject = payloadSubject(payload)
		out = append(out, it)
	}
	return out, rows.Err()
}

// payloadSubject is a message's subject (or the first line of its body or
// text) for a one-line listing; "" when it has none.
func payloadSubject(payload string) string {
	var p map[string]any
	if json.Unmarshal([]byte(payload), &p) != nil {
		return ""
	}
	for _, key := range []string{"subject", "body", "text"} {
		v, _ := p[key].(string)
		if line, _, _ := strings.Cut(strings.TrimSpace(v), "\n"); line != "" {
			if r := []rune(line); len(r) > 80 {
				line = string(r[:79]) + "…"
			}
			return line
		}
	}
	return ""
}
