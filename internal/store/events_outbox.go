package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/events"
)

// The events outbox (CW-20261016-0136, X1a). Every events row gets a D6 event id
// when it is written. A row written on a caller's transaction is also pending
// until the bus has fanned it out after the commit; PendingEventsContext /
// MarkPublishedContext are the drainer's view of that.

var (
	_ events.EventIDPersister = (*Store)(nil)
	_ events.TxPersister      = (*Store)(nil)
)

// SetEventsCommittedHook installs the function called by EventsCommitted. The
// composition root wires it to the bus's KickOutbox.
func (s *Store) SetEventsCommittedHook(fn func()) {
	if fn == nil {
		s.eventsCommitted.Store(nil)
		return
	}
	s.eventsCommitted.Store(&fn)
}

// EventsCommitted must be called after a transaction that wrote outbox rows
// commits: it wakes the drainer so the rows are published now rather than on
// its next sweep. Safe with no hook installed.
func (s *Store) EventsCommitted() {
	if fn := s.eventsCommitted.Load(); fn != nil {
		(*fn)()
	}
}

type eventWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// insertEventRow writes one events row on w (a transaction or the pool) with a D6
// id (minted unless eventID is given). pending marks it for the outbox drainer.
// A repeat of an event id already stored writes nothing and returns the stored
// row's sequence: retries are idempotent on the id.
func insertEventRow(ctx context.Context, w eventWriter, scope events.Scope, sessionID, kind, payloadJSON string, at string, pending bool) (int64, string, error) {
	seq, id, _, err := insertEventRowExisting(ctx, w, scope, sessionID, kind, payloadJSON, at, "", pending)
	return seq, id, err
}

// insertEventRowExisting is insertEventRow that also reports whether the event id
// was already stored (in which case nothing was written).
func insertEventRowExisting(ctx context.Context, w eventWriter, scope events.Scope, sessionID, kind, payloadJSON string, at string, eventID string, pending bool) (int64, string, bool, error) {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return 0, "", false, fmt.Errorf("event time %q: %w", at, err)
	}
	stored, existing, err := insertEventEnvelopeRow(ctx, w, events.Event{Scope: scope, SessionID: sessionID, Kind: kind, PayloadJSON: payloadJSON}, t, eventID, pending)
	return stored.Seq, stored.EventID, existing, err
}

// InsertEventTx implements events.TxPersister: it writes e as a pending row on
// the caller's transaction. The environment-request kind needs its own status
// bookkeeping and is not supported here.
func (s *Store) InsertEventTx(ctx context.Context, tx events.Tx, e events.Event) (events.Event, error) {
	if e.Scope == "" {
		return events.Event{}, fmt.Errorf("event scope required")
	}
	if e.Kind == EnvironmentRequestKind {
		return events.Event{}, fmt.Errorf("event kind %q cannot be written on a caller's transaction", e.Kind)
	}
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	stored, existing, err := insertEventEnvelopeRow(ctx, tx, e, at, e.EventID, true)
	if err != nil {
		return events.Event{}, err
	}
	if existing {
		// A repeat of an id already stored is that row, not what this caller sent.
		rows, err := tx.QueryContext(ctx, `SELECT `+eventReadColumns+` FROM events WHERE id = ?`, stored.Seq)
		if err != nil {
			return events.Event{}, err
		}
		defer rows.Close()
		again, err := scanEventsWithID(rows)
		if err != nil {
			return events.Event{}, fmt.Errorf("event %s already stored but unreadable: %w", stored.EventID, err)
		}
		if len(again) != 1 {
			return events.Event{}, fmt.Errorf("event %s already stored but unreadable", stored.EventID)
		}
		return again[0], nil
	}
	return stored, nil
}

// PendingEventsContext implements events.TxPersister.
func (s *Store) PendingEventsContext(ctx context.Context, beforeSeq int64, limit int) ([]events.Event, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventReadColumns+` FROM events WHERE pending = 1 AND (? <= 0 OR id < ?) ORDER BY id ASC LIMIT ?`, beforeSeq, beforeSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventsWithID(rows)
}

// MarkPublishedContext implements events.TxPersister.
func (s *Store) MarkPublishedContext(ctx context.Context, seqs []int64) error {
	const chunk = 400
	for len(seqs) > 0 {
		n := min(chunk, len(seqs))
		args := make([]any, n)
		for i, seq := range seqs[:n] {
			args[i] = seq
		}
		//nolint:gosec // G202: placeholders only; values are bound
		if _, err := s.db.ExecContext(ctx, `UPDATE events SET pending = 0 WHERE pending = 1 AND id IN (`+strings.TrimSuffix(strings.Repeat("?,", n), ",")+`)`, args...); err != nil {
			return err
		}
		seqs = seqs[n:]
	}
	return nil
}

// scanEventsWithID hydrates eventReadColumns, including the producer ID.
func scanEventsWithID(rows *sql.Rows) ([]events.Event, error) {
	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func newEventID() string { return runtimeevents.NewEventID() }
