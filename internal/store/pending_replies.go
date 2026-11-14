package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

// PendingReplyWindow is how long an unanswered typed message keeps waiting; an
// older one no longer gates a seat (it is stale, not forgotten: it stays in
// the mailbox).
const PendingReplyWindow = 7 * 24 * time.Hour

// PendingReply is an inbound typed message that waits on the recipient
// (CW-20261114-0013): the Stop gate asks for these before letting a turn end.
type PendingReply struct {
	MessageID string        `json:"message_id"`
	From      string        `json:"from"`
	Kind      envelope.Kind `json:"kind"`
	Subject   string        `json:"subject,omitempty"`
}

// PendingReplies lists typed messages to agentURN that need its answer: those
// awaiting handle, and questions, decisions and approval requests that await
// nobody in particular. Reading one does not settle it (CW-20261114-0013
// settle rule): it stops pending when the agent replies to it (in_reply_to),
// when it is canceled or archived, or after PendingReplyWindow.
func (s *Store) PendingReplies(ctx context.Context, agentURN, handle string, limit int) ([]PendingReply, error) {
	since := time.Now().UTC().Add(-PendingReplyWindow).Format(time.RFC3339Nano)
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	// NeedsReply in SQL, so a backlog of unread updates cannot crowd real
	// questions out of the limit; the Go check below is the same rule.
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.from_urn, coalesce(m.payload,'') FROM messages m
 WHERE m.to_urn=? AND m.content_type=? AND m.created_at > ? AND m.canceled_at IS NULL AND m.archived_at IS NULL AND m.routing_staged=0
   AND json_valid(m.payload)
   AND (json_extract(m.payload,'$.awaiting') = ?
        OR (coalesce(json_extract(m.payload,'$.awaiting'),'') = '' AND json_extract(m.payload,'$.kind') IN ('question','decision','approval_request')))
   AND NOT EXISTS (SELECT 1 FROM messages r WHERE r.in_reply_to=m.id AND r.from_urn=?)
 ORDER BY m.created_at LIMIT ?`, agentURN, envelope.ContentType, since, handle, agentURN, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingReply{}
	for rows.Next() && len(out) < limit {
		var id, from, payload string
		if err := rows.Scan(&id, &from, &payload); err != nil {
			return nil, err
		}
		var e envelope.Envelope
		if json.Unmarshal([]byte(payload), &e) != nil {
			continue
		}
		if !NeedsReply(e, handle) {
			continue
		}
		out = append(out, PendingReply{MessageID: id, From: from, Kind: e.Kind, Subject: e.Subject})
	}
	return out, rows.Err()
}

// NeedsReply reports whether e waits on the holder of handle.
func NeedsReply(e envelope.Envelope, handle string) bool {
	if e.Awaiting != "" {
		return e.Awaiting == handle
	}
	return envelope.HasOptions(e.Kind)
}
