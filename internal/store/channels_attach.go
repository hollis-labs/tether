package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	gomsg "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

var _ channels.ExistingMessageBackend = (*Store)(nil)

// AttachChannelMessage authorizes the session publisher before atomically
// releasing its staged envelope, indexing publication and recording router audit.
// It never sends or copies a body and never enqueues a delivery obligation.
func (s *Store) AttachChannelMessage(ctx context.Context, req channels.ExistingMessage) (gomsg.Envelope, error) {
	target, err := channels.ChannelAddress(req.Channel)
	if err != nil {
		return gomsg.Envelope{}, err
	}
	if req.MessageID == "" || req.SessionID == "" || req.Actor.IsZero() {
		return gomsg.Envelope{}, channels.ErrInvalid
	}
	if _, err := gomsg.ParseURN(req.Actor.URN()); err != nil {
		return gomsg.Envelope{}, channels.ErrInvalid
	}
	sender := gomsg.Address{Kind: gomsg.KindSession, Authority: "local", ID: req.SessionID}
	if _, err := gomsg.ParseURN(sender.URN()); err != nil {
		return gomsg.Envelope{}, channels.ErrInvalid
	}
	env, staged, err := attachmentEnvelope(ctx, s.db, req.MessageID)
	if err != nil {
		return gomsg.Envelope{}, err
	}
	if staged && routingStageExpired(env.CreatedAt.Format(time.RFC3339Nano)) {
		return gomsg.Envelope{}, gomsg.ErrNotFound
	}
	if env.From != sender {
		return gomsg.Envelope{}, channels.ErrInvalid
	}
	route, err := s.SessionRoute(ctx, req.SessionID)
	if err != nil {
		return gomsg.Envelope{}, err
	}
	if route == nil || route.Channel != req.Channel {
		return gomsg.Envelope{}, channels.ErrInvalid
	}
	if staged && (!slices.Contains(route.Kinds, env.Metadata["kind"]) || env.Metadata["session_id"] != req.SessionID) {
		return gomsg.Envelope{}, channels.ErrInvalid
	}
	env.To, env.Channel, env.ThreadID = target, gomsg.Channel(req.Channel), req.SessionID
	if err := s.authorizeChannelPublication(ctx, env); err != nil {
		return gomsg.Envelope{}, err
	}
	row, err := s.GetSession(req.SessionID)
	if err != nil {
		return gomsg.Envelope{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return gomsg.Envelope{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	current, staged, err := attachmentEnvelope(ctx, tx, req.MessageID)
	if err != nil {
		return gomsg.Envelope{}, err
	}
	if current.From != sender {
		return gomsg.Envelope{}, channels.ErrInvalid
	}
	if !staged {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM channel_publications WHERE message_id=?`, req.MessageID).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return gomsg.Envelope{}, channels.ErrInvalid
			}
			return gomsg.Envelope{}, err
		}
		if name != req.Channel || current.To != target || current.ThreadID != req.SessionID {
			return gomsg.Envelope{}, channels.ErrInvalid
		}
		return current, nil
	}
	if len(current.Payload) == 0 || routingStageExpired(current.CreatedAt.Format(time.RFC3339Nano)) {
		return gomsg.Envelope{}, gomsg.ErrNotFound
	}
	if current.Metadata["kind"] != env.Metadata["kind"] || current.Metadata["session_id"] != req.SessionID {
		return gomsg.Envelope{}, channels.ErrInvalid
	}
	current.To, current.Channel, current.ThreadID = target, gomsg.Channel(req.Channel), req.SessionID
	current.Metadata["launch_id"] = row.LaunchID
	display := req.LaunchDisplayName
	if display == "" {
		display = row.LaunchID
	}
	current.Metadata["launch_display_name"] = display
	metadata, err := json.Marshal(current.Metadata)
	if err != nil {
		return gomsg.Envelope{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE messages SET to_urn=?,channel=?,thread_id=?,metadata=?,routing_staged=0
 WHERE id=? AND routing_staged=1 AND payload IS NOT NULL AND from_urn=?`, target.URN(), req.Channel, req.SessionID, string(metadata), req.MessageID, sender.URN())
	if err != nil {
		return gomsg.Envelope{}, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return gomsg.Envelope{}, err
	}
	if count != 1 {
		return gomsg.Envelope{}, gomsg.ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO channel_publications(name,message_id) VALUES(?,?)`, req.Channel, req.MessageID); err != nil {
		return gomsg.Envelope{}, err
	}
	audit, _ := json.Marshal(map[string]string{"actor": req.Actor.URN(), "publisher": sender.URN(), "session_id": req.SessionID, "message_id": req.MessageID, "turn_id": current.Metadata["turn_id"], "channel": req.Channel})
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(scope,session_id,at,kind,payload_json) VALUES(?,?,?,?,?)`, events.ScopeSession, req.SessionID, time.Now().UTC().Format(time.RFC3339Nano), events.KindSessionTurnRouted, string(audit)); err != nil {
		return gomsg.Envelope{}, fmt.Errorf("audit channel attachment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return gomsg.Envelope{}, err
	}
	return current, nil
}

type attachmentReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func attachmentEnvelope(ctx context.Context, reader attachmentReader, id string) (gomsg.Envelope, bool, error) {
	var staged bool
	row := reader.QueryRowContext(ctx, `SELECT id,kind,channel,from_urn,to_urn,thread_id,in_reply_to,
 payload,content_type,metadata,created_at,delivered_at,consumed_at,routing_staged FROM messages WHERE id=?`, id)
	env, err := scanEnvelope(func(dest ...any) error { return row.Scan(append(dest, &staged)...) })
	if errors.Is(err, sql.ErrNoRows) {
		return gomsg.Envelope{}, false, gomsg.ErrNotFound
	}
	return env, staged, err
}
