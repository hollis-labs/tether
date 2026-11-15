package msgstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"github.com/google/uuid"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
)

// TurnOutputAddress is the recipient of every staged turn output.
var TurnOutputAddress = messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "turn-output"}

// ValidOutputID reports whether id is a turn output identity: 64 lowercase hex characters.
func ValidOutputID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// StageTurnOutput stores a session's turn output as a staged (routing_staged = 1) message to
// TurnOutputAddress, with a deterministic id: from output_id when the metadata carries one, else
// from (session, turn, kind), else a fresh UUIDv7. A repeat with the same body returns the stored
// row. A different body under the same identity is stored separately under an id derived from the
// body when there is no output_id (and logged), and refused when there is one.
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
		env.Metadata = map[string]string{}
	}
	env.Metadata["session_id"] = env.From.ID
	key, _ := json.Marshal([3]string{env.From.ID, env.Metadata["turn_id"], env.Metadata["kind"]})
	outputID := env.Metadata["output_id"]
	switch {
	case outputID != "":
		if !ValidOutputID(outputID) {
			return messaging.Envelope{}, errors.New("stage turn output: invalid output identity")
		}
		key, _ = json.Marshal([2]string{env.From.ID, outputID})
		env.ID = uuid.NewSHA1(uuid.NameSpaceURL, append([]byte("tether:turn-output:id:"), key...)).String()
	case env.Metadata["turn_id"] == "":
		id, err := s.NewMessageID()
		if err != nil {
			return messaging.Envelope{}, err
		}
		env.ID = id
	default:
		env.ID = uuid.NewSHA1(uuid.NameSpaceURL, append([]byte("tether:turn-output:"), key...)).String()
	}
	env.CreatedAt, env.Kind, env.To = s.now(), messaging.MsgKindNotice, TurnOutputAddress
	saved, err := s.insertStaged(ctx, env)
	if err != nil {
		return messaging.Envelope{}, err
	}
	if bytes.Equal(saved.Payload, env.Payload) && saved.ContentType == env.ContentType {
		return saved, nil
	}
	if outputID != "" {
		return messaging.Envelope{}, fmt.Errorf("stage turn output: conflicting or purged output identity %q", outputID)
	}
	alternate, _ := json.Marshal([5]string{env.From.ID, env.Metadata["turn_id"], env.Metadata["kind"], string(env.Payload), env.ContentType})
	env.ID = uuid.NewSHA1(uuid.NameSpaceURL, append([]byte("tether:turn-output:body:"), alternate...)).String()
	slog.Error("stage turn output: reused output identity with a different body; preserving it separately",
		"session", env.From.ID, "turn", env.Metadata["turn_id"], "stored", saved.ID, "alternate", env.ID)
	saved, err = s.insertStaged(ctx, env)
	if err != nil {
		return messaging.Envelope{}, err
	}
	if !bytes.Equal(saved.Payload, env.Payload) || saved.ContentType != env.ContentType {
		return messaging.Envelope{}, fmt.Errorf("stage turn output: conflicting or purged alternate body %q", env.ID)
	}
	return saved, nil
}

// insertStaged writes the staged row unless the id exists (INSERT OR IGNORE in the old store) and
// returns what is stored under the id.
func (s *Store) insertStaged(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error) {
	meta, err := json.Marshal(env.Metadata)
	if err != nil {
		return messaging.Envelope{}, err
	}
	var saved messaging.Envelope
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := LockMessageSeq(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO messages
 (id, kind, channel, from_urn, to_urn, thread_id, payload, content_type, metadata, created_at, routing_staged)
 VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, 1) ON CONFLICT (id) DO NOTHING`), env.ID, string(env.Kind), env.From.URN(), env.To.URN(),
			nullText(env.ThreadID), string(env.Payload), env.ContentType, string(meta), stamp(env.CreatedAt)); err != nil {
			return fmt.Errorf("stage turn output: %w", err)
		}
		var err error
		saved, err = scanEnvelope(tx.QueryRowContext(ctx, rebind(tx, `SELECT `+envelopeColumns+` FROM messages WHERE id = ?`), env.ID).Scan)
		return err
	})
	return saved, err
}

// StagedTurnOutput returns a staged turn output that still has its body.
func (s *Store) StagedTurnOutput(ctx context.Context, id string) (messaging.Envelope, error) {
	r := s.c.Reader()
	env, err := scanEnvelope(r.QueryRowContext(ctx, rebind(r, `SELECT `+envelopeColumns+` FROM messages
 WHERE id = ? AND routing_staged = 1 AND payload IS NOT NULL`), id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return messaging.Envelope{}, messaging.ErrNotFound
	}
	return env, err
}

// PendingTurnOutput is a staged output still inside the retention window.
type PendingTurnOutput struct {
	MessageID string
	SessionID string // the local session that produced it ("" when the sender is not one)
	CreatedAt string // UTC, fixed-width nine-digit fraction (the paging key)
}

// PendingTurnOutputCursor pages PendingTurnOutputs.
type PendingTurnOutputCursor struct {
	CreatedAt string
	MessageID string
}

// fixedWidth is the paging key's format: created_at with exactly nine fraction digits.
const fixedWidth = "2006-01-02T15:04:05.000000000Z"

// stagedAt normalizes created_at (RFC 3339 UTC, 0 to 9 fraction digits) to fixedWidth with the
// same text functions on both dialects.
const stagedAt = `substr(created_at,1,19) || '.' ||
 CASE WHEN substr(created_at,20,1) = '.' THEN substr(substr(created_at,21,length(created_at)-21) || '000000000',1,9)
 ELSE '000000000' END || 'Z'`

// PendingTurnOutputs returns staged outputs with a body, newer than RoutingStageRetention, after
// the cursor in (created_at, id) order (limit 1..1000, default 128). The old store filtered the
// window with julianday(), which is SQLite-only; created_at is always UTC, so comparing the
// fixed-width text with a cutoff in the same format is the same test on both dialects.
func (s *Store) PendingTurnOutputs(ctx context.Context, after PendingTurnOutputCursor, limit int) ([]PendingTurnOutput, error) {
	if limit <= 0 || limit > 1000 {
		limit = 128
	}
	cutoff := s.now().Add(-RoutingStageRetention).Format(fixedWidth)
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `WITH pending AS (
 SELECT id, from_urn, `+stagedAt+` AS staged_at FROM messages WHERE routing_staged = 1 AND payload IS NOT NULL
) SELECT id, from_urn, staged_at FROM pending
 WHERE staged_at > ? AND (staged_at > ? OR (staged_at = ? AND id > ?)) ORDER BY staged_at, id LIMIT ?`),
		cutoff, after.CreatedAt, after.CreatedAt, after.MessageID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PendingTurnOutput
	for rows.Next() {
		var item PendingTurnOutput
		var from string
		if err := rows.Scan(&item.MessageID, &from, &item.CreatedAt); err != nil {
			return nil, err
		}
		if sender, err := messaging.ParseURN(from); err == nil && sender.Kind == messaging.KindSession && sender.Authority == "local" {
			item.SessionID = sender.ID
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
