package store

import (
	"context"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
)

type PendingTurnOutput struct {
	MessageID string
	SessionID string
}

// PendingTurnOutputs pages the durable retry queue without loading bodies.
// Every complete sweep starts at the beginning, so a failed stage is never
// stranded behind a checkpoint. Purged stages are intentionally excluded.
func (s *Store) PendingTurnOutputs(ctx context.Context, afterID string, limit int) ([]PendingTurnOutput, error) {
	if limit <= 0 || limit > 1000 {
		limit = 128
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,from_urn FROM messages
 WHERE routing_staged=1 AND payload IS NOT NULL AND id>? AND julianday(created_at)>julianday(?) ORDER BY id LIMIT ?`, afterID, time.Now().UTC().Add(-RoutingStageRetention).Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []PendingTurnOutput
	for rows.Next() {
		var item PendingTurnOutput
		var from string
		if err := rows.Scan(&item.MessageID, &from); err != nil {
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
