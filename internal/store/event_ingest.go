package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// EventByEventID returns the stored event with the producer-minted id.
func (s *Store) EventByEventID(ctx context.Context, eventID string) (events.Event, bool, error) {
	e, err := scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventReadColumns+` FROM events WHERE event_id = ?`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return events.Event{}, false, nil
	}
	if err != nil {
		return events.Event{}, false, err
	}
	return e, true, nil
}

// IngestDigest returns the digest recorded for an ingested event id. found is
// false when none was recorded (an event written before the ingest existed, or
// by a crash between the insert and the record).
func (s *Store) IngestDigest(ctx context.Context, eventID string) (digest string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT digest FROM event_ingest_digests WHERE event_id = ?`, eventID).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return digest, err == nil, err
}

// IngestOutcome is how IngestEvent settled an id.
type IngestOutcome int

const (
	// IngestStored: the event and its digest were committed together.
	IngestStored IngestOutcome = iota
	// IngestDuplicate: the id is stored with the same digest.
	IngestDuplicate
	// IngestConflict: the id is stored with a different digest, or with no digest
	// at all. Missing evidence is never taken for a verified duplicate.
	IngestConflict
)

// IngestEvent writes e and the digest of what the producer sent in one
// transaction, so an event never exists without its digest. The bus fans the
// event out after the commit (its outbox drainer), not before the digest is
// durable. A repeat of a stored id writes nothing: the same digest is a
// duplicate, anything else a conflict. The returned event is the stored one.
func (s *Store) IngestEvent(ctx context.Context, bus events.OutboxBus, e events.Event, digest, app string, at time.Time) (events.Event, IngestOutcome, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return events.Event{}, IngestStored, err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventReadColumns+` FROM events WHERE event_id = ?`, e.EventID))
	switch {
	case err == nil:
		var recorded string
		err := tx.QueryRowContext(ctx, `SELECT digest FROM event_ingest_digests WHERE event_id = ?`, e.EventID).Scan(&recorded)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && recorded != digest) {
			return existing, IngestConflict, nil
		}
		if err != nil {
			return events.Event{}, IngestStored, err
		}
		return existing, IngestDuplicate, nil
	case !errors.Is(err, sql.ErrNoRows):
		return events.Event{}, IngestStored, err
	}

	stored, err := bus.PublishTx(ctx, tx, e)
	if err != nil {
		return events.Event{}, IngestStored, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO event_ingest_digests (event_id, digest, app, at) VALUES (?, ?, ?, ?)`,
		e.EventID, digest, app, at.UTC().Format(time.RFC3339Nano)); err != nil {
		return events.Event{}, IngestStored, err
	}
	if err := tx.Commit(); err != nil {
		return events.Event{}, IngestStored, err
	}
	bus.KickOutbox()
	return stored, IngestStored, nil
}
