package eventstore

import (
	"context"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
)

// RetentionBatch is one age-based sweep batch and its retention_audit receipt.
type RetentionBatch struct {
	Table   string    `json:"table"`
	Cutoff  time.Time `json:"cutoff"`
	Removed int64     `json:"removed"`
	AuditID int64     `json:"audit_id"`
}

// historySweeps are the tables DeleteHistoryBefore may sweep, with the class of the swept table.
// ai_events is gone with the LLM gateway (decision D3: archived, not migrated).
var historySweeps = map[string]struct {
	query string
	class store.Class
}{
	"events":               {`DELETE FROM events WHERE id IN (SELECT id FROM events WHERE at < ? ORDER BY at, id LIMIT ?)`, store.B},
	"events:debug":         {`DELETE FROM events WHERE id IN (SELECT id FROM events WHERE retention_class = 'R-debug' AND at < ? ORDER BY id LIMIT ?)`, store.B},
	"identity_audit":       {`DELETE FROM identity_audit WHERE id IN (SELECT id FROM identity_audit WHERE at < ? ORDER BY at, id LIMIT ?)`, store.B},
	"event_ingest_digests": {`DELETE FROM event_ingest_digests WHERE event_id IN (SELECT event_id FROM event_ingest_digests WHERE at < ? ORDER BY at, event_id LIMIT ?)`, store.B},
	"turn_inputs":          {`DELETE FROM turn_inputs WHERE input_id IN (SELECT input_id FROM turn_inputs WHERE submitted_at < ? ORDER BY submitted_at, input_id LIMIT ?)`, store.B},
	"proxy_events":         {`DELETE FROM proxy_events WHERE id IN (SELECT id FROM proxy_events WHERE timestamp < ? ORDER BY timestamp, id LIMIT ?)`, store.B},
	"a2a_tasks": {`DELETE FROM a2a_tasks WHERE (binding_id, task_id) IN (
 SELECT binding_id, task_id FROM a2a_tasks
 WHERE state IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED')
 AND updated_ns < ? ORDER BY updated_ns, binding_id, task_id LIMIT ?)`, store.C},
}

// DeleteEventsBefore sweeps one batch of events older than cutoff.
func (s *Store) DeleteEventsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	b, err := s.DeleteHistoryBefore(ctx, "events", cutoff, limit)
	return b.Removed, err
}

// DeleteHistoryBefore deletes up to limit rows of table older than cutoff and, when it removed any,
// writes a retention_audit receipt in the same transaction. For a class B table that is one class B
// transaction. a2a_tasks is class C, so its sweep is the allowlisted joint "DeleteEventHistoryBefore"
// (C rows plus the B receipt), until the S8 retention redesign separates them.
func (s *Store) DeleteHistoryBefore(ctx context.Context, table string, cutoff time.Time, limit int) (RetentionBatch, error) {
	batch := RetentionBatch{Table: table, Cutoff: cutoff}
	if limit <= 0 {
		return batch, fmt.Errorf("retention: limit must be positive")
	}
	sweep, ok := historySweeps[table]
	if !ok {
		return batch, fmt.Errorf("retention: unsupported table %q", table)
	}
	stamp := formatTime(cutoff)
	var age any = stamp
	if table == "a2a_tasks" {
		age = cutoff.UnixNano()
	}
	fn := func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, sweep.query), age, limit)
		if err != nil {
			return err
		}
		if batch.Removed, err = res.RowsAffected(); err != nil {
			return err
		}
		if batch.Removed == 0 {
			return nil
		}
		return tx.QueryRowContext(ctx, rebind(tx, `INSERT INTO retention_audit (at, table_name, cutoff, removed) VALUES (?, ?, ?, ?) RETURNING id`),
			formatTime(s.now()), table, stamp, batch.Removed).Scan(&batch.AuditID)
	}
	var err error
	if sweep.class == store.B {
		err = s.b.WriteTx(ctx, fn)
	} else {
		err = s.all.Joint(ctx, "DeleteEventHistoryBefore", fn)
	}
	if err != nil {
		return RetentionBatch{Table: table, Cutoff: cutoff}, err
	}
	return batch, nil
}

// TombstoneCandidate is an event whose payload may be replaced by its digest.
type TombstoneCandidate struct {
	Seq               int64
	RecordedAt        time.Time
	RetentionClass    string
	RetentionPolicyID string
	PolicyJSON        string // the stored policy snapshot ("" when the policy row is missing)
	PayloadJSON       string
	PII               int64 // the raw flag (0 or 1 is valid; anything else is a corrupt row)
	Sensitivity       string
}

// TombstoneDecision is the event slice's verdict on one candidate.
type TombstoneDecision struct {
	Tombstone bool
	Digest    string // payload_digest to store ("withheld" or "sha256:<hex>")
}

// TombstoneBatch is one pass over the candidates.
type TombstoneBatch struct {
	Scanned    int
	Tombstoned int
	NextCursor int64
}

// TombstonePayloads is the storage half of the old TombstoneEventPayloads: it reads up to limit
// settled events after the cursor that still hold a payload under a retention policy, asks decide
// about each (the retention rules, policy-snapshot verification and digest are the event slice's,
// internal/eventretention in the old tree), and replaces the payload by the digest where decide says
// so. The UPDATE is guarded by the values read, so a row changed in between is left alone. The pass
// stops at the first error from decide, with nothing written.
func (s *Store) TombstonePayloads(ctx context.Context, after int64, limit int,
	decide func(TombstoneCandidate) (TombstoneDecision, error)) (TombstoneBatch, error) {
	if after < 0 || limit <= 0 || limit > 1000 || decide == nil {
		return TombstoneBatch{}, fmt.Errorf("retention: require a decide function, a nonnegative cursor and limit in [1,1000]")
	}
	result := TombstoneBatch{NextCursor: after}
	err := s.b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		result = TombstoneBatch{NextCursor: after}
		rows, err := tx.QueryContext(ctx, rebind(tx, `SELECT e.id, e.recorded_at, e.retention_class, e.retention_policy_id,
 COALESCE(p.policy_json,''), e.payload_json, COALESCE(e.pii,0), COALESCE(e.sensitivity,'')
 FROM events e LEFT JOIN event_retention_policies p ON p.id = e.retention_policy_id
 WHERE e.id > ? AND e.pending = 0 AND e.payload_json IS NOT NULL AND e.payload_digest IS NULL AND e.recorded_at IS NOT NULL
 AND e.retention_class IS NOT NULL AND e.retention_policy_id IS NOT NULL ORDER BY e.id LIMIT ?`), after, limit)
		if err != nil {
			return err
		}
		var selected []TombstoneCandidate
		for rows.Next() {
			var c TombstoneCandidate
			var recorded string
			if err := rows.Scan(&c.Seq, &recorded, &c.RetentionClass, &c.RetentionPolicyID, &c.PolicyJSON, &c.PayloadJSON, &c.PII, &c.Sensitivity); err != nil {
				_ = rows.Close()
				return err
			}
			if c.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded); err != nil {
				_ = rows.Close()
				return fmt.Errorf("retention: event %d has an invalid recorded_at: %w", c.Seq, err)
			}
			selected = append(selected, c)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range selected {
			d, err := decide(c)
			if err != nil {
				return err
			}
			result.Scanned++
			result.NextCursor = c.Seq
			if !d.Tombstone {
				continue
			}
			res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE events SET payload_json = NULL, payload_digest = ?
 WHERE id = ? AND pending = 0 AND payload_digest IS NULL AND payload_json = ? AND retention_policy_id = ? AND retention_class = ?`),
				d.Digest, c.Seq, c.PayloadJSON, c.RetentionPolicyID, c.RetentionClass)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			result.Tombstoned += int(n)
		}
		return nil
	})
	if err != nil {
		return TombstoneBatch{}, err
	}
	return result, nil
}
