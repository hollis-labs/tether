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
// (limit default 256). The CAST types the first parameter as bigint on Postgres (bare, it would be
// int4 and refuse a seq above 2^31).
func (s *Store) Pending(ctx context.Context, beforeSeq int64, limit int) ([]Row, error) {
	if limit <= 0 {
		limit = 256
	}
	return s.query(ctx, `SELECT `+readColumns+` FROM events WHERE pending = 1 AND (CAST(? AS BIGINT) <= 0 OR id < ?) ORDER BY id ASC LIMIT ?`,
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
	// raced is set when the default insert finds the event already written by a path other than
	// ingest (between the existence check and the insert), so the event was not ingested here.
	var raced bool
	if insert == nil {
		insert = func(ctx context.Context, tx store.Tx, r Row) (Row, error) {
			stored, existing, err := s.InsertTx(ctx, tx, r, true)
			raced = existing
			return stored, err
		}
	}
	var out Row
	var outcome IngestOutcome
	err := s.b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		out, outcome = Row{}, IngestStored
		// compare reports an event that is already stored: Duplicate or Conflict against the digest
		// recorded at its ingest, Unverifiable when it was not ingested (no digest row).
		compare := func(existing Row) error {
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
		}
		existing, err := scanRow(tx.QueryRowContext(ctx, rebind(tx, `SELECT `+readColumns+` FROM events WHERE event_id = ?`), r.EventID))
		switch {
		case err == nil:
			return compare(existing)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		// Claim the id before writing the event. On Postgres (READ COMMITTED) a concurrent ingest of
		// the same id waits here on the first one's digest row and, once it commits, inserts nothing
		// and reports against the stored event, instead of failing on the digest's primary key. On
		// SQLite the one writer already serializes the two.
		res, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO event_ingest_digests (event_id, digest, app, at) VALUES (?, ?, ?, ?)
 ON CONFLICT (event_id) DO NOTHING`), r.EventID, digest, app, formatTime(at))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			existing, err := scanRow(tx.QueryRowContext(ctx, rebind(tx, `SELECT `+readColumns+` FROM events WHERE event_id = ?`), r.EventID))
			if err != nil {
				return fmt.Errorf("eventstore: ingest %q: digest claimed without its event: %w", r.EventID, err)
			}
			return compare(existing)
		}
		raced = false
		if out, err = insert(ctx, tx, r); err != nil || !raced {
			return err
		}
		outcome = IngestUnverifiable
		_, err = tx.ExecContext(ctx, rebind(tx, `DELETE FROM event_ingest_digests WHERE event_id = ?`), r.EventID)
		return err
	})
	if err != nil {
		return Row{}, IngestStored, err
	}
	return out, outcome, nil
}
