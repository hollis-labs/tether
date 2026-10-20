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

// RecordIngestDigest stores the digest for an ingested event id. A repeat of
// the same id keeps the first digest.
func (s *Store) RecordIngestDigest(ctx context.Context, eventID, digest, app string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO event_ingest_digests (event_id, digest, app, at) VALUES (?, ?, ?, ?)
		ON CONFLICT(event_id) DO NOTHING`, eventID, digest, app, at.UTC().Format(time.RFC3339Nano))
	return err
}
