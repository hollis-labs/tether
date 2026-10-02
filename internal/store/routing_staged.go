package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	messaging "github.com/hollis-labs/go-messaging"
)

// RoutingStageRetention is the normal 30-day retention window for an output
// whose channel publication never completes. Purging remains an explicit
// operator action; staging must not keep orphaned text indefinitely.
const RoutingStageRetention = 30 * 24 * time.Hour

// StageTurnOutput persists an envelope without delivering it. The router later
// attaches this exact message to a channel. Do not use MessagingStore.Send:
// that enqueues delivery obligations and fans out to inbox subscribers.
func (s *Store) StageTurnOutput(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error) {
	if env.From.Kind != messaging.KindSession || env.From.Authority != "local" {
		return messaging.Envelope{}, fmt.Errorf("stage turn output: local session sender required")
	}
	if _, err := messaging.ParseURN(env.From.URN()); err != nil {
		return messaging.Envelope{}, err
	}
	if env.ThreadID != "" && env.ThreadID != env.From.ID {
		return messaging.Envelope{}, fmt.Errorf("stage turn output: thread must be its session")
	}
	if recorded := env.Metadata["session_id"]; recorded != "" && recorded != env.From.ID {
		return messaging.Envelope{}, fmt.Errorf("stage turn output: session metadata must match sender")
	}
	env.ThreadID = env.From.ID
	env.Metadata = maps.Clone(env.Metadata)
	if env.Metadata == nil {
		env.Metadata = make(map[string]string)
	}
	env.Metadata["session_id"] = env.From.ID
	// JSON encodes the tuple without delimiter ambiguity. The existing message
	// primary key makes repeated staging idempotent, including after attachment.
	key, _ := json.Marshal([3]string{env.From.ID, env.Metadata["turn_id"], env.Metadata["kind"]})
	env.ID = uuid.NewSHA1(uuid.NameSpaceURL, append([]byte("tether:turn-output:"), key...)).String()
	env.CreatedAt = time.Now().UTC()
	env.Kind = messaging.MsgKindNotice
	env.To = messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "turn-output"}
	env.Channel = ""
	env.DeliveredAt, env.ConsumedAt = nil, nil
	meta, err := json.Marshal(env.Metadata)
	if err != nil {
		return messaging.Envelope{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO messages
 (id, kind, channel, from_urn, to_urn, thread_id, payload, content_type, metadata, created_at, routing_staged)
 VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, 1)`, env.ID, string(env.Kind), env.From.URN(), env.To.URN(),
		nullIfEmpty(env.ThreadID), string(env.Payload), env.ContentType, string(meta), env.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("stage turn output: %w", err)
	}
	// Return the original body and timestamp if this was a repeated call.
	row := s.db.QueryRowContext(ctx, `SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
 payload, content_type, metadata, created_at, delivered_at, consumed_at FROM messages WHERE id=?`, env.ID)
	return scanEnvelope(row.Scan)
}

// StagedTurnOutput is the internal router read. Public Get/list/inbox reads
// deliberately cannot see these messages. Purged output cannot be attached.
func (s *Store) StagedTurnOutput(ctx context.Context, id string) (messaging.Envelope, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, kind, channel, from_urn, to_urn, thread_id, in_reply_to,
 payload, content_type, metadata, created_at, delivered_at, consumed_at
 FROM messages WHERE id=? AND routing_staged=1 AND payload IS NOT NULL`, id)
	env, err := scanEnvelope(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return messaging.Envelope{}, messaging.ErrNotFound
	}
	return env, err
}

func routingStageExpired(created string) bool {
	t := parseMsgTime(created)
	return !t.IsZero() && !t.After(time.Now().UTC().Add(-RoutingStageRetention))
}
