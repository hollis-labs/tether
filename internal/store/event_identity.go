package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
)

var (
	ErrEventIdentityMismatch = errors.New("event log environment identity differs from persisted identity")
	ErrEventIdentityUnseeded = errors.New("event log environment identity is not seeded")
	ErrEventIDsAfterCutover  = errors.New("event log contains NULL event IDs above the frozen cutover; restore a supported rollback build")
)

// EventIdentityProgress is durable backfill progress. Dropped is always zero:
// an error stops the pass; neither events nor their IDs are discarded.
type EventIdentityProgress struct {
	EnvironmentID string `json:"environment_id"`
	Cutover       int64  `json:"cutover"`
	NextLegacyID  int64  `json:"next_legacy_id"`
	Remaining     int64  `json:"remaining"`
	Backfilled    int64  `json:"backfilled"`
	Failed        int64  `json:"failed"`
	Dropped       int64  `json:"dropped"`
}

const eventIdentityColumns = `environment_id,cutover,next_legacy_id,remaining,backfilled,failed`

func scanEventIdentity(row eventScanner) (EventIdentityProgress, error) {
	var p EventIdentityProgress
	err := row.Scan(&p.EnvironmentID, &p.Cutover, &p.NextLegacyID, &p.Remaining, &p.Backfilled, &p.Failed)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrEventIdentityUnseeded
	}
	return p, err
}

// SeedEventIdentity must run at the coordinated stored-ID cutover, after the
// environment file is read and before any event producers start. Reopening the
// same DB verifies identity without moving the cutover or resetting progress.
// The initial count is one scan; later progress reads use only the singleton.
func (s *Store) SeedEventIdentity(ctx context.Context, environmentID string) (EventIdentityProgress, error) {
	id, err := uuid.Parse(environmentID)
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != environmentID {
		return EventIdentityProgress{}, errors.New("invalid environment UUIDv4")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventIdentityProgress{}, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := scanEventIdentity(tx.QueryRowContext(ctx, `SELECT `+eventIdentityColumns+` FROM event_identity WHERE singleton=1`))
	if err == nil {
		if p.EnvironmentID != environmentID {
			return EventIdentityProgress{}, ErrEventIdentityMismatch
		}
		var invalid bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE id>? AND event_id IS NULL)`, p.Cutover).Scan(&invalid); err != nil {
			return EventIdentityProgress{}, err
		}
		if invalid {
			return EventIdentityProgress{}, ErrEventIDsAfterCutover
		}
		return p, tx.Commit()
	}
	if !errors.Is(err, ErrEventIdentityUnseeded) {
		return EventIdentityProgress{}, err
	}
	var cutover, remaining int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0),COUNT(CASE WHEN event_id IS NULL THEN 1 END) FROM events`).Scan(&cutover, &remaining); err != nil {
		return EventIdentityProgress{}, err
	}
	next := cutover
	if remaining == 0 {
		next = 0
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO event_identity(singleton,environment_id,cutover,next_legacy_id,remaining) VALUES(1,?,?,?,?)`, environmentID, cutover, next, remaining); err != nil {
		return EventIdentityProgress{}, err
	}
	p = EventIdentityProgress{EnvironmentID: environmentID, Cutover: cutover, NextLegacyID: next, Remaining: remaining}
	return p, tx.Commit()
}

func (s *Store) EventIdentityProgress(ctx context.Context) (EventIdentityProgress, error) {
	return scanEventIdentity(s.db.QueryRowContext(ctx, `SELECT `+eventIdentityColumns+` FROM event_identity WHERE singleton=1`))
}

// BackfillEventIDs repairs at most limit legacy rows, newest first, in a single
// transaction. IDs already present (including early evt_ IDs) remain unchanged.
// The cursor and counters commit with the IDs, so interruption is resumable.
// Callers yield between chunks and stop on errors; no busy loop belongs here.
func (s *Store) BackfillEventIDs(ctx context.Context, limit int) (EventIdentityProgress, error) {
	if limit < 1 || limit > 1000 {
		return EventIdentityProgress{}, errors.New("event ID backfill limit must be between 1 and 1000")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventIdentityProgress{}, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := scanEventIdentity(tx.QueryRowContext(ctx, `SELECT `+eventIdentityColumns+` FROM event_identity WHERE singleton=1`))
	if err != nil {
		return EventIdentityProgress{}, err
	}
	if p.NextLegacyID == 0 {
		return p, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM events WHERE id<=? AND id<=? AND event_id IS NULL ORDER BY id DESC LIMIT ?`, p.NextLegacyID, p.Cutover, limit)
	if err != nil {
		return EventIdentityProgress{}, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return EventIdentityProgress{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return EventIdentityProgress{}, err
	}
	if closeErr != nil {
		return EventIdentityProgress{}, closeErr
	}
	var changed int64
	if len(ids) > 0 {
		args := []any{p.EnvironmentID + ":", p.Cutover}
		for _, id := range ids {
			args = append(args, id)
		}
		//nolint:gosec // Only placeholders are built here; identity, bound and IDs are bound parameters.
		result, err := tx.ExecContext(ctx, `UPDATE events SET event_id=? || CAST(id AS TEXT) WHERE event_id IS NULL AND id<=? AND id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
		if err != nil {
			return EventIdentityProgress{}, err
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return EventIdentityProgress{}, err
		}
		p.NextLegacyID = ids[len(ids)-1] - 1
	} else {
		p.NextLegacyID = 0
	}
	// Retention can have deleted legacy rows since seeding. Reconcile remaining
	// once at completion; do not invent or skip IDs to repair a discrepancy.
	p.Backfilled += changed
	p.Remaining -= changed
	if p.Remaining < 0 {
		return EventIdentityProgress{}, errors.New("event ID backfill progress underflow")
	}
	if p.Remaining == 0 {
		p.NextLegacyID = 0
	}
	if p.NextLegacyID == 0 {
		p.Remaining = 0
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_identity SET next_legacy_id=?,remaining=?,backfilled=? WHERE singleton=1`, p.NextLegacyID, p.Remaining, p.Backfilled); err != nil {
		return EventIdentityProgress{}, err
	}
	return p, tx.Commit()
}

// RecordEventIDBackfillFailure counts a failed chunk after its transaction has
// rolled back. It stores no error text, payload, or event content.
func (s *Store) RecordEventIDBackfillFailure(ctx context.Context) error {
	result, err := s.db.ExecContext(ctx, `UPDATE event_identity SET failed=failed+1 WHERE singleton=1`)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrEventIdentityUnseeded
	}
	return nil
}
