package store

import (
	"context"
	"encoding/json"
	"time"
)

// RecordMessageWake records identifiers and typed outcome only. Free-form
// provider detail, wake text and message metadata are never journaled.
func (s *Store) RecordMessageWake(ctx context.Context, id, target, kind, attemptID string, attempt int, generation int64, attempted, delivered bool, reason string) error {
	if kind != "wake.attempted" && kind != "wake.outcome" {
		return nil
	}
	// Reasons are fixed boundary labels; arbitrary provider/error strings do not
	// become lifecycle payloads even if a future caller passes them accidentally.
	if !messageWakeReasons[reason] {
		reason = "unknown"
	}
	payload := map[string]any{"message_id": id, "target": target, "attempted": attempted, "delivered": delivered, "reason": reason}
	if attemptID != "" {
		payload["attempt_id"] = attemptID
		payload["attempt"] = attempt
		payload["binding_generation"] = generation
	}
	if generation > 0 {
		payload["binding_generation"] = generation
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO message_event_journal(kind,at,payload_json,team_run_id) SELECT ?,?,?,coalesce((SELECT run_id FROM team_runs WHERE run_id=(SELECT CASE WHEN json_valid(metadata) THEN json_extract(metadata,'$.team_run') END FROM messages WHERE id=?)), '') WHERE EXISTS (SELECT 1 FROM messages WHERE id=? AND kind NOT LIKE 'stream.%')`, kind, time.Now().UTC().Format(time.RFC3339Nano), string(raw), id, id)
	return err
}

var messageWakeReasons = map[string]bool{
	"": true, "pull-only": true, "stale-generation": true, "binding-check-failed": true,
	"already-handled":       true,
	"busy":                  true,
	"claim-unavailable":     true,
	"invalid-wake-intent":   true,
	"marker-write-failed":   true,
	"no-wake":               true,
	"offline":               true,
	"offline-race":          true,
	"recipient-unavailable": true,
	"settle-failed":         true,
	"turn-submit-failed":    true,
	"untracked-delivery":    true,
	"wake-admission-failed": true,
	"wake-outcome-unknown":  true,
}
