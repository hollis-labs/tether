package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/eventretention"
	"github.com/hollis-labs/tether/internal/events"
)

// SetEventRetentionAdmission atomically replaces the trusted kind catalog and
// saves its immutable resolved policy. It affects future admissions only. No
// daemon/catalog caller invokes this yet; an empty mapping disables new class
// admission but does not alter policies already referenced by events.
func (s *Store) SetEventRetentionAdmission(ctx context.Context, cfg eventretention.Config, kinds map[string]eventretention.Class) error {
	if err := cfg.ValidateForPass(); err != nil {
		return err
	}
	encoded, err := cfg.EncodeSnapshot()
	if err != nil {
		return err
	}
	for kind, class := range kinds {
		if strings.TrimSpace(kind) == "" {
			return fmt.Errorf("retention: empty catalog kind")
		}
		if _, err := cfg.Snapshot(class); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO event_retention_policies(id,policy_json,created_at) VALUES (?,?,?) ON CONFLICT(id) DO NOTHING`, cfg.ID(), string(encoded), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	// Verify an existing row rather than letting a corrupt/colliding ID silently
	// reinterpret all its old events under a different configuration.
	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT policy_json FROM event_retention_policies WHERE id=?`, cfg.ID()).Scan(&existing); err != nil {
		return err
	}
	if existing != string(encoded) {
		return fmt.Errorf("retention: policy snapshot content mismatch")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_retention_admission`); err != nil {
		return err
	}
	for kind, class := range kinds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_retention_admission(kind,retention_class,policy_id) VALUES (?,?,?)`, kind, "R-"+string(class), cfg.ID()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// applyEventRetentionAdmission is used by the one shared INSERT path, with the
// caller's transaction/connection. Producer claims cannot choose a destructive
// retention policy. No second connection or bus lock is acquired here.
func applyEventRetentionAdmission(ctx context.Context, w eventWriter, e *events.Event) error {
	if err := e.Envelope.Validate(); err != nil {
		return err
	}
	switch e.Envelope.Class.Sensitivity {
	case "", "public", "private":
	case "secret":
		return fmt.Errorf("retention: secret events must not be stored")
	default:
		return fmt.Errorf("retention: unknown event sensitivity")
	}
	e.Envelope.RetentionClass = ""
	e.Envelope.RetentionPolicyID = ""
	var class, policyID string
	err := w.QueryRowContext(ctx, `SELECT a.retention_class,a.policy_id FROM event_retention_admission a JOIN event_retention_policies p ON p.id=a.policy_id WHERE a.kind=?`, e.Kind).Scan(&class, &policyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	e.Envelope.RetentionClass = class
	e.Envelope.RetentionPolicyID = policyID
	return nil
}

// EventTombstoneBatch describes only committed payload removals. NextCursor is
// a scan cursor for this sweep, not a deletion checkpoint: a later sweep must
// start again at zero to reconsider younger or previously pending payloads.
type EventTombstoneBatch struct {
	Scanned    int
	Tombstoned int
	NextCursor int64
}

// TombstoneEventPayloads scans at most limit classified, published payloads and
// removes only payloads whose current policy permits it. Admission snapshots are
// audit provenance, not retention decisions. Snapshot integrity, metadata and
// pending guards are checked in the same transaction as the conditional UPDATE.
// No row is deleted and no envelope/cursor metadata is modified.
func (s *Store) TombstoneEventPayloads(ctx context.Context, current eventretention.Config, now time.Time, after int64, limit int) (EventTombstoneBatch, error) {
	if now.IsZero() || after < 0 || limit <= 0 || limit > 1000 {
		return EventTombstoneBatch{}, fmt.Errorf("retention: require now, nonnegative cursor and limit in [1,1000]")
	}
	if err := current.ValidateForPass(); err != nil {
		return EventTombstoneBatch{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventTombstoneBatch{}, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id,recorded_at,retention_class,retention_policy_id,payload_json,COALESCE(pii,0),COALESCE(sensitivity,'') FROM events
 WHERE id>? AND pending=0 AND payload_json IS NOT NULL AND payload_digest IS NULL AND recorded_at IS NOT NULL AND retention_class IS NOT NULL AND retention_policy_id IS NOT NULL ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return EventTombstoneBatch{}, err
	}
	type candidate struct {
		id                                      int64
		at, class, policy, payload, sensitivity string
		pii                                     int
	}
	selected := []candidate{}
	for rows.Next() {
		var r candidate
		if err := rows.Scan(&r.id, &r.at, &r.class, &r.policy, &r.payload, &r.pii, &r.sensitivity); err != nil {
			_ = rows.Close()
			return EventTombstoneBatch{}, err
		}
		selected = append(selected, r)
	}
	if err := rows.Close(); err != nil {
		return EventTombstoneBatch{}, err
	}
	if err := rows.Err(); err != nil {
		return EventTombstoneBatch{}, err
	}
	result := EventTombstoneBatch{NextCursor: after}
	verified := map[string]bool{}
	for _, r := range selected {
		if !verified[r.policy] {
			var raw string
			if err := tx.QueryRowContext(ctx, `SELECT policy_json FROM event_retention_policies WHERE id=?`, r.policy).Scan(&raw); err != nil {
				return EventTombstoneBatch{}, fmt.Errorf("retention policy lookup: %w", err)
			}
			cfg, err := eventretention.RestoreSnapshot([]byte(raw))
			if err != nil {
				return EventTombstoneBatch{}, err
			}
			if cfg.ID() != r.policy {
				return EventTombstoneBatch{}, fmt.Errorf("retention: policy ID does not match stored snapshot")
			}
			verified[r.policy] = true
		}
		policy, err := current.Snapshot(eventretention.Class(strings.TrimPrefix(r.class, "R-")))
		if err != nil {
			return EventTombstoneBatch{}, err
		}
		at, err := time.Parse(time.RFC3339Nano, r.at)
		if err != nil {
			return EventTombstoneBatch{}, fmt.Errorf("retention: invalid recorded_at: %w", err)
		}
		if r.pii != 0 && r.pii != 1 {
			return EventTombstoneBatch{}, fmt.Errorf("retention: invalid pii flag")
		}
		if r.sensitivity != "" && r.sensitivity != "public" && r.sensitivity != "private" {
			return EventTombstoneBatch{}, fmt.Errorf("retention: forbidden or unknown sensitivity")
		}
		decision, err := eventretention.Plan(eventretention.Record{Cursor: r.id, RecordedAt: at, Policy: policy, PII: r.pii == 1, PayloadPresent: true}, now)
		if err != nil {
			return EventTombstoneBatch{}, err
		}
		result.Scanned++
		result.NextCursor = r.id
		if !decision.Tombstone {
			continue
		}
		_, digest := eventretention.Tombstone([]byte(r.payload), r.pii == 1)
		update, err := tx.ExecContext(ctx, `UPDATE events SET payload_json=NULL,payload_digest=? WHERE id=? AND pending=0 AND payload_digest IS NULL AND payload_json=? AND retention_policy_id=? AND retention_class=?`, digest, r.id, r.payload, r.policy, r.class)
		if err != nil {
			return EventTombstoneBatch{}, err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return EventTombstoneBatch{}, err
		}
		result.Tombstoned += int(changed)
	}
	if err := tx.Commit(); err != nil {
		return EventTombstoneBatch{}, err
	}
	return result, nil
}
