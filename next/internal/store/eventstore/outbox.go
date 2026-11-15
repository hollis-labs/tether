package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
)

// Pending returns outbox rows not yet published, oldest first, before beforeSeq when it is > 0
// (limit default 256).
func (s *Store) Pending(ctx context.Context, beforeSeq int64, limit int) ([]Row, error) {
	if limit <= 0 {
		limit = 256
	}
	return s.query(ctx, `SELECT `+readColumns+` FROM events WHERE pending = 1 AND (? <= 0 OR id < ?) ORDER BY id ASC LIMIT ?`,
		beforeSeq, beforeSeq, limit)
}

// MarkPublished clears the pending flag on seqs, in chunks of 400 ids per statement.
func (s *Store) MarkPublished(ctx context.Context, seqs []int64) error {
	const chunk = 400
	return s.b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		for rest := seqs; len(rest) > 0; {
			n := min(chunk, len(rest))
			args := make([]any, n)
			for i, seq := range rest[:n] {
				args[i] = seq
			}
			//nolint:gosec // G202: placeholders only; the values are bound
			if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE events SET pending = 0 WHERE pending = 1 AND id IN (`+placeholders(n)+`)`), args...); err != nil {
				return err
			}
			rest = rest[n:]
		}
		return nil
	})
}

// IngestOutcome is the result of IngestEvent.
type IngestOutcome int

const (
	// IngestStored: a new event, stored as an outbox row with its digest.
	IngestStored IngestOutcome = iota
	// IngestDuplicate: the event id is stored with the same digest.
	IngestDuplicate
	// IngestConflict: the event id is stored with a different digest.
	IngestConflict
	// IngestUnverifiable: the event id is stored but has no digest to compare (not ingested).
	IngestUnverifiable
)

// IngestDigest returns the digest recorded for an ingested event id.
func (s *Store) IngestDigest(ctx context.Context, eventID string) (digest string, found bool, err error) {
	r := s.b.Reader()
	err = r.QueryRowContext(ctx, rebind(r, `SELECT digest FROM event_ingest_digests WHERE event_id = ?`), eventID).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return digest, err == nil, err
}

// IngestEvent stores an event published by another app through POST /events (the hub side,
// CW-20261020-0010): in one class B transaction it checks for the event id, writes the event as an
// outbox row and records the payload digest with the publishing app. insert is how the event is
// written (the event slice's publisher, which stamps the envelope); nil writes r through InsertTx as
// a pending row. The caller kicks the outbox after a stored outcome.
func (s *Store) IngestEvent(ctx context.Context, r Row, digest, app string, at time.Time,
	insert func(ctx context.Context, tx store.Tx, r Row) (Row, error)) (Row, IngestOutcome, error) {
	if r.EventID == "" {
		return Row{}, IngestStored, fmt.Errorf("eventstore: an ingested event needs its event id")
	}
	if insert == nil {
		insert = func(ctx context.Context, tx store.Tx, r Row) (Row, error) {
			stored, _, err := s.InsertTx(ctx, tx, r, true)
			return stored, err
		}
	}
	var out Row
	outcome := IngestStored
	err := s.b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		existing, err := scanRow(tx.QueryRowContext(ctx, rebind(tx, `SELECT `+readColumns+` FROM events WHERE event_id = ?`), r.EventID))
		switch {
		case err == nil:
			out = existing
			var recorded string
			err := tx.QueryRowContext(ctx, rebind(tx, `SELECT digest FROM event_ingest_digests WHERE event_id = ?`), r.EventID).Scan(&recorded)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				outcome = IngestUnverifiable
			case err != nil:
				return err
			case recorded != digest:
				outcome = IngestConflict
			default:
				outcome = IngestDuplicate
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if out, err = insert(ctx, tx, r); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, rebind(tx, `INSERT INTO event_ingest_digests (event_id, digest, app, at) VALUES (?, ?, ?, ?)`),
			r.EventID, digest, app, formatTime(at))
		return err
	})
	if err != nil {
		return Row{}, IngestStored, err
	}
	return out, outcome, nil
}
