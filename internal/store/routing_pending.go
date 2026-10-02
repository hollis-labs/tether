package store

import (
	"context"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
)

type PendingTurnOutput struct {
	MessageID string
	SessionID string
	CreatedAt string // UTC timestamp with a fixed-width fractional second
}

type PendingTurnOutputCursor struct {
	CreatedAt string
	MessageID string
}

// PendingTurnOutputs pages the durable retry queue without loading bodies.
// Every complete sweep starts at the beginning, so a failed stage is never
// stranded behind a checkpoint. Purged stages are intentionally excluded.
func (s *Store) PendingTurnOutputs(ctx context.Context, after PendingTurnOutputCursor, limit int) ([]PendingTurnOutput, error) {
	if limit <= 0 || limit > 1000 {
		limit = 128
	}
	// Stages use UTC RFC3339Nano. Pad legacy trimmed fractions before comparing
	// text, so .1Z sorts before .1001Z and whole seconds sort before fractions.
	rows, err := s.db.QueryContext(ctx, `WITH pending AS (
 SELECT id,from_urn,substr(created_at,1,19)||'.'||
 CASE WHEN substr(created_at,20,1)='.' THEN
 substr(substr(created_at,21,length(created_at)-21)||'000000000',1,9)
 ELSE '000000000' END||'Z' AS staged_at
 FROM messages WHERE routing_staged=1 AND payload IS NOT NULL
 AND julianday(created_at)>julianday(?)
 ) SELECT id,from_urn,staged_at FROM pending
 WHERE staged_at>? OR (staged_at=? AND id>?) ORDER BY staged_at,id LIMIT ?`,
		time.Now().UTC().Add(-RoutingStageRetention).Format(time.RFC3339Nano), after.CreatedAt, after.CreatedAt, after.MessageID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []PendingTurnOutput
	for rows.Next() {
		var item PendingTurnOutput
		var from string
		if err := rows.Scan(&item.MessageID, &from, &item.CreatedAt); err != nil {
			return nil, err
		}
		sender, err := gomsg.ParseURN(from)
		if err == nil && sender.Kind == gomsg.KindSession && sender.Authority == "local" {
			item.SessionID = sender.ID
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
