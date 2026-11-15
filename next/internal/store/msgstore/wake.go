package msgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
)

// AdmitRecipientWake claims the one wake of a message's recipient. admitted=true for the first
// caller; later callers get the stored outcome (empty until CompleteRecipientWake). A message that
// does not exist is messaging.ErrNotFound.
func (s *Store) AdmitRecipientWake(ctx context.Context, id string) (admitted bool, outcome string, err error) {
	var n int64
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO recipient_wake_admissions (message_id, admitted_at)
 SELECT id, ? FROM messages WHERE id = ? ON CONFLICT (message_id) DO NOTHING`), stamp(s.now()), id)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return false, "", err
	}
	if n == 1 {
		return true, "", nil
	}
	got, found, err := s.RecipientWakeOutcome(ctx, id)
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "", messaging.ErrNotFound
	}
	return false, got, nil
}

// CompleteRecipientWake records the outcome of an admitted wake.
func (s *Store) CompleteRecipientWake(ctx context.Context, id, outcome string) error {
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, rebind(tx, `UPDATE recipient_wake_admissions SET outcome_json = ? WHERE message_id = ?`), outcome, id)
		return err
	})
}

// RecipientWakeOutcome returns the stored outcome of a message's wake, found=false when it was
// never admitted.
func (s *Store) RecipientWakeOutcome(ctx context.Context, id string) (string, bool, error) {
	r := s.c.Reader()
	var result sql.NullString
	err := r.QueryRowContext(ctx, rebind(r, `SELECT outcome_json FROM recipient_wake_admissions WHERE message_id = ?`), id).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return result.String, err == nil, err
}

// MessageWakeReasons are the reasons a wake event may carry; anything else is recorded as
// "unknown".
var MessageWakeReasons = map[string]bool{
	"": true, "pull-only": true, "stale-generation": true, "binding-check-failed": true,
	"already-handled": true, "busy": true, "claim-unavailable": true, "invalid-wake-intent": true,
	"marker-write-failed": true, "no-wake": true, "offline": true, "offline-race": true,
	"recipient-unavailable": true, "settle-failed": true, "turn-submit-failed": true,
	"untracked-delivery": true, "wake-admission-failed": true, "wake-outcome-unknown": true,
	"triaged-fyi": true, "triaged-read": true, "triaged-covered": true, "deferred-cap": true,
}

// RecordMessageWake journals a wake.attempted or wake.outcome event for a message (other kinds are
// ignored), with the team run named in the message's metadata when that run exists, and nothing for
// a stream.* message or a message that does not exist.
func (s *Store) RecordMessageWake(ctx context.Context, id, target, kind, attemptID string, attempt int, generation int64, attempted, delivered bool, reason string) error {
	if kind != "wake.attempted" && kind != "wake.outcome" {
		return nil
	}
	if !MessageWakeReasons[reason] {
		reason = "unknown"
	}
	payload := map[string]any{"message_id": id, "target": target, "attempted": attempted, "delivered": delivered, "reason": reason}
	if attemptID != "" {
		payload["attempt_id"], payload["attempt"], payload["binding_generation"] = attemptID, attempt, generation
	}
	if generation > 0 {
		payload["binding_generation"] = generation
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// The team run is read from the message's metadata. On Postgres the cast is guarded by
		// pg_input_is_valid(..., 'jsonb'), the schema trigger's own guard (IS JSON accepts text jsonb
		// refuses), after each \u0000 escape is replaced with U+FFFD (Go writes one for a NUL; jsonb
		// refuses it), so such metadata still yields its team_run as on SQLite. Stream kinds are
		// matched case-insensitively on both (SQLite's LIKE folds ASCII case; ILIKE, as the triggers).
		teamRun := `CASE WHEN json_valid(metadata) THEN json_extract(metadata,'$.team_run') END`
		notStream := `kind NOT LIKE 'stream.%'`
		if tx.Dialect().Name() == gostore.DriverPostgres {
			md := pgUnNul(`metadata`)
			teamRun = `CASE WHEN pg_input_is_valid(` + md + `, 'jsonb') THEN (` + md + `)::jsonb->>'team_run' END`
			notStream = `kind NOT ILIKE 'stream.%'`
		}
		_, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO message_event_journal (kind, at, payload_json, team_run_id)
 SELECT ?, ?, ?, COALESCE((SELECT run_id FROM team_runs WHERE run_id = (SELECT `+teamRun+` FROM messages WHERE id = ?)), '')
 WHERE EXISTS (SELECT 1 FROM messages WHERE id = ? AND `+notStream+`)`), kind, stamp(s.now()), string(raw), id, id)
		return err
	})
}

// Wake triage states.
const (
	TriagePending  = "pending"  // classified, not yet acted on
	TriageWoken    = "woken"    // a wake turn was submitted for it
	TriageCovered  = "covered"  // another wake of the same seat announced it
	TriageSurfaced = "surfaced" // fyi: listed on a human-originated turn
	TriageSettled  = "settled"  // read: marked read by the hub
)

// TriageRow is one hub-side classification of mail to a seat before it may wake the seat.
type TriageRow struct {
	MessageID, Recipient, Seat, Class, Reason string
	Typed                                     bool
	State                                     string
	NotBefore, CreatedAt                      time.Time
}

// TriageGet returns a message's triage row.
func (s *Store) TriageGet(ctx context.Context, messageID string) (TriageRow, bool, error) {
	r := s.c.Reader()
	var t TriageRow
	var typed int64
	var notBefore sql.NullString
	var created string
	err := r.QueryRowContext(ctx, rebind(r, `SELECT message_id, recipient, seat, class, reason, typed, state, not_before, created_at FROM wake_triage WHERE message_id = ?`), messageID).
		Scan(&t.MessageID, &t.Recipient, &t.Seat, &t.Class, &t.Reason, &typed, &t.State, &notBefore, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return TriageRow{}, false, nil
	}
	if err != nil {
		return TriageRow{}, false, err
	}
	t.Typed = typed == 1
	if notBefore.Valid {
		t.NotBefore, _ = time.Parse(time.RFC3339Nano, notBefore.String)
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return t, true, nil
}

// TriageRecord stores a classification once (the first one wins) and returns the stored row;
// created=true when this call stored it. An empty state is pending.
func (s *Store) TriageRecord(ctx context.Context, t TriageRow) (stored TriageRow, created bool, err error) {
	if t.State == "" {
		t.State = TriagePending
	}
	typed := int64(0)
	if t.Typed {
		typed = 1
	}
	at := stamp(t.CreatedAt)
	var n int64
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO wake_triage (message_id, recipient, seat, class, reason, typed, state, created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (message_id) DO NOTHING`), t.MessageID, t.Recipient, t.Seat, t.Class, t.Reason, typed, t.State, at, at)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return t, false, err
	}
	stored, _, err = s.TriageGet(ctx, t.MessageID)
	return stored, n == 1, err
}

func (s *Store) execC(ctx context.Context, q string, args ...any) (int64, error) {
	var n int64
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, q), args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// TriageSetState moves a triage row to state. It has no state guard and SetSeatLastActionWake is
// last-writer-wins, as in the old store; on a Postgres pool these writes really run concurrently, so
// the mail_triage caller (S6) must not rely on them to order racing wakes.
func (s *Store) TriageSetState(ctx context.Context, messageID, state string, at time.Time) error {
	_, err := s.execC(ctx, `UPDATE wake_triage SET state = ?, updated_at = ? WHERE message_id = ?`, state, stamp(at), messageID)
	return err
}

// TriageDefer sets not_before on a pending triage row.
func (s *Store) TriageDefer(ctx context.Context, messageID string, notBefore, at time.Time) error {
	_, err := s.execC(ctx, `UPDATE wake_triage SET not_before = ?, updated_at = ? WHERE message_id = ? AND state = 'pending'`, stamp(notBefore), stamp(at), messageID)
	return err
}

// TriageCoverPending marks the seat's other pending action items covered and returns how many.
func (s *Store) TriageCoverPending(ctx context.Context, seat, exceptMessageID string, at time.Time) (int64, error) {
	return s.execC(ctx, `UPDATE wake_triage SET state = 'covered', updated_at = ? WHERE seat = ? AND state = 'pending' AND class = 'action' AND message_id <> ?`,
		stamp(at), seat, exceptMessageID)
}

// SeatLastActionWake returns when the seat was last woken for an action item (zero when never).
func (s *Store) SeatLastActionWake(ctx context.Context, seat string) (time.Time, error) {
	r := s.c.Reader()
	var raw string
	err := r.QueryRowContext(ctx, rebind(r, `SELECT last_action_wake_at FROM seat_wake_clock WHERE seat = ?`), seat).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339Nano, raw)
	return t, nil
}

// SetSeatLastActionWake records the seat's last action wake.
func (s *Store) SetSeatLastActionWake(ctx context.Context, seat string, at time.Time) error {
	_, err := s.execC(ctx, `INSERT INTO seat_wake_clock (seat, last_action_wake_at) VALUES (?, ?)
 ON CONFLICT (seat) DO UPDATE SET last_action_wake_at = excluded.last_action_wake_at`, seat, stamp(at))
	return err
}

// FYIItem is a pending fyi item to list on a seat's next human-originated turn.
type FYIItem struct {
	MessageID, Recipient, From string
	Typed                      bool
	Subject                    string
}

// TriageFYIPending returns the seat's pending fyi items whose messages are still unread, live and
// unarchived, oldest first (limit default 20).
func (s *Store) TriageFYIPending(ctx context.Context, seat string, limit int) ([]FYIItem, error) {
	if limit <= 0 {
		limit = 20
	}
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT t.message_id, t.recipient, m.from_urn, t.typed, COALESCE(m.payload,'')
 FROM wake_triage t JOIN messages m ON m.id = t.message_id
 WHERE t.seat = ? AND t.class = 'fyi' AND t.state = 'pending' AND m.read_at IS NULL AND m.canceled_at IS NULL AND m.archived_at IS NULL
 ORDER BY t.created_at, t.message_id LIMIT ?`), seat, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FYIItem
	for rows.Next() {
		var it FYIItem
		var typed int64
		var payload string
		if err := rows.Scan(&it.MessageID, &it.Recipient, &it.From, &typed, &payload); err != nil {
			return nil, err
		}
		it.Typed, it.Subject = typed == 1, payloadSubject(payload)
		out = append(out, it)
	}
	return out, rows.Err()
}

// payloadSubject is the first line of the payload's subject, body or text, at most 80 runes.
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
