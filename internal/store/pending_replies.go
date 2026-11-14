package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

// PendingReply is an inbound typed message that waits on the recipient
// (CW-20261114-0013): the Stop gate asks for these before letting a turn end.
type PendingReply struct {
	MessageID string        `json:"message_id"`
	From      string        `json:"from"`
	Kind      envelope.Kind `json:"kind"`
	Subject   string        `json:"subject,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

// PendingReplies lists typed messages to agentURN that need its answer: those
// awaiting handle, and questions, decisions and approval requests that await
// nobody in particular. Reading one does not settle it (CW-20261114-0013
// settle rule): it stops pending when the agent replies to it (in_reply_to)
// or when it is canceled or archived. Age does not settle it; the Stop gate
// only blocks on recent ones and the watchdog owns the rest.
//
// agentURNs are every address the seat is known by (its catalog/registry URN and
// the msg://agent/agent-mux/<id> binding target): mail to any of them counts,
// and a reply from any of them settles it.
func (s *Store) PendingReplies(ctx context.Context, agentURNs []string, handle string, limit int) ([]PendingReply, error) {
	if len(agentURNs) == 0 {
		return []PendingReply{}, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(agentURNs)), ",")
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	// NeedsReply in SQL, so a backlog of unread updates cannot crowd real
	// questions out of the limit; the Go check below is the same rule.
	//nolint:gosec // G202: `in` is only "?" placeholders built from a count; every value is a bound argument
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.from_urn, coalesce(m.payload,''), m.created_at FROM messages m
 WHERE m.to_urn IN (`+in+`) AND m.content_type=? AND m.canceled_at IS NULL AND m.archived_at IS NULL AND m.routing_staged=0
   AND json_valid(m.payload)
   AND (json_extract(m.payload,'$.awaiting') = ?
        OR (coalesce(json_extract(m.payload,'$.awaiting'),'') = '' AND json_extract(m.payload,'$.kind') IN ('question','decision','approval_request')))
   AND NOT EXISTS (SELECT 1 FROM messages r WHERE r.in_reply_to=m.id AND r.from_urn IN (`+in+`))
 ORDER BY m.created_at LIMIT ?`, pendingArgs(agentURNs, handle, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingReply{}
	for rows.Next() && len(out) < limit {
		var id, from, payload, created string
		if err := rows.Scan(&id, &from, &payload, &created); err != nil {
			return nil, err
		}
		var e envelope.Envelope
		if json.Unmarshal([]byte(payload), &e) != nil {
			continue
		}
		if !NeedsReply(e, handle) {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, created)
		out = append(out, PendingReply{MessageID: id, From: from, Kind: e.Kind, Subject: e.Subject, CreatedAt: at})
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

func pendingArgs(urns []string, handle string, limit int) []any {
	args := make([]any, 0, 2*len(urns)+3)
	for _, u := range urns {
		args = append(args, u)
	}
	args = append(args, envelope.ContentType, handle)
	for _, u := range urns {
		args = append(args, u)
	}
	return append(args, limit)
}
