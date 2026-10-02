package store

import (
	"context"
	"fmt"
	"time"
)

// RetentionBatch describes committed removals. AuditID identifies the durable
// receipt; no row bodies are copied into the audit. Archiving is a separate job.
type RetentionBatch struct {
	Table   string    `json:"table"`
	Cutoff  time.Time `json:"cutoff"`
	Removed int64     `json:"removed"`
	AuditID int64     `json:"audit_id"`
}

// DeleteEventHistoryBefore deletes one bounded batch and writes its audit
// receipt atomically. Accepted tables are event history, identity audit and
// terminal A2A tasks. Other task states are never eligible, regardless of age.
func (s *Store) DeleteEventHistoryBefore(ctx context.Context, table string, cutoff time.Time, limit int) (RetentionBatch, error) {
	batch := RetentionBatch{Table: table, Cutoff: cutoff}
	if limit <= 0 {
		return batch, fmt.Errorf("retention: limit must be positive")
	}
	stamp := cutoff.UTC().Format(time.RFC3339Nano)
	var age any = stamp
	var query string
	switch table {
	case "a2a_tasks":
		// State values are persisted from a2a.TaskState; these are the SDK
		// Terminal() states. Use the last update, not the task creation time.
		query = `DELETE FROM a2a_tasks WHERE (binding_id, task_id) IN (
			SELECT binding_id, task_id FROM a2a_tasks
			WHERE state IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED')
			AND updated_ns < ? ORDER BY updated_ns, binding_id, task_id LIMIT ?)`
		age = cutoff.UnixNano()
	case "identity_audit":
		query = `DELETE FROM identity_audit WHERE id IN (SELECT id FROM identity_audit WHERE at < ? ORDER BY at, id LIMIT ?)`
	case "events":
		query = `DELETE FROM events WHERE id IN (SELECT id FROM events WHERE at < ? ORDER BY at, id LIMIT ?)`
	case "proxy_events":
		query = `DELETE FROM proxy_events WHERE id IN (SELECT id FROM proxy_events WHERE timestamp < ? ORDER BY timestamp, id LIMIT ?)`
	case "ai_events":
		query = `DELETE FROM ai_events WHERE id IN (SELECT id FROM ai_events WHERE timestamp < ? ORDER BY timestamp, id LIMIT ?)`
	default:
		return batch, fmt.Errorf("retention: unsupported table %q", table)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return batch, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, query, age, limit)
	if err != nil {
		return batch, err
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return batch, err
	}
	var auditID int64
	if removed > 0 {
		res, err = tx.ExecContext(ctx, `INSERT INTO retention_audit (at, table_name, cutoff, removed) VALUES (?, ?, ?, ?)`, time.Now().UTC().Format(time.RFC3339Nano), table, stamp, removed)
		if err != nil {
			return batch, err
		}
		auditID, err = res.LastInsertId()
		if err != nil {
			return batch, err
		}
	}
	if err := tx.Commit(); err != nil {
		return batch, err
	}
	batch.Removed, batch.AuditID = removed, auditID
	return batch, nil
}
