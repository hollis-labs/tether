package store

import (
	"context"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// upsertWorkStateSQL records a session's current work-state only when the event
// that carries it is newer than the one already recorded, so a replayed or late
// event can never overwrite a newer value.
const upsertWorkStateSQL = `INSERT INTO environment_work_state(session_id,value,since,event_seq) VALUES(?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET value=excluded.value,since=excluded.since,event_seq=excluded.event_seq WHERE excluded.event_seq>environment_work_state.event_seq`

// insertWorkStateEnvelope stores a session.work_state_changed event and its
// current-value row in one transaction: either both exist or neither does. An
// event whose payload names no known value is still stored (the log is
// append-only and never rejects a kind) but leaves the row untouched.
func (s *Store) insertWorkStateEnvelope(ctx context.Context, e events.Event) (events.Event, error) {
	w, ok := parseWorkState(e.PayloadJSON)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return events.Event{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, _, err := insertEventEnvelopeRow(ctx, tx, e, time.Now().UTC(), "", false)
	if err != nil {
		return events.Event{}, err
	}
	if ok && e.SessionID != "" {
		since := w.At
		if since.IsZero() {
			since = stored.At
		}
		if _, err = tx.ExecContext(ctx, upsertWorkStateSQL, e.SessionID, w.Value, since.UTC().Format(time.RFC3339Nano), stored.Seq); err != nil {
			return events.Event{}, fmt.Errorf("record work-state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return events.Event{}, err
	}
	return stored, nil
}

// insertWorkState is the legacy (scope, session, kind, payload) entry point.
func (s *Store) insertWorkState(ctx context.Context, scope events.Scope, sessionID, payload string) (int64, time.Time, string, error) {
	stored, err := s.insertWorkStateEnvelope(ctx, events.Event{Scope: scope, SessionID: sessionID, Kind: events.KindSessionWorkStateChanged, PayloadJSON: payload})
	return stored.Seq, stored.At, stored.EventID, err
}
