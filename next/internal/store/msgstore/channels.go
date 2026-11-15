package msgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
)

// ErrAttachInvalid is returned when the attachment request does not match the stored message (channels.ErrInvalid
// in the old tree; the channel slice maps it).
var ErrAttachInvalid = errors.New("channel attachment does not match the message")

// ChannelURN is a channel's address (msg://service/local/channel/<name>). Validating the name is
// the channel slice's.
func ChannelURN(name string) string {
	return messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "channel", SubID: name}.URN()
}

// ListChannelNames returns every channel with a publication, sorted.
func (s *Store) ListChannelNames(ctx context.Context) ([]string, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, `SELECT DISTINCT name FROM channel_publications ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// ChannelHighWater is the channel's highest publication seq (0 when empty).
func (s *Store) ChannelHighWater(ctx context.Context, name string) (int64, error) {
	r := s.c.Reader()
	var seq int64
	err := r.QueryRowContext(ctx, rebind(r, `SELECT COALESCE(MAX(seq), 0) FROM channel_publications WHERE name = ?`), name).Scan(&seq)
	return seq, err
}

// ChannelMessage is a publication with its seq and purge state.
type ChannelMessage struct {
	Seq      int64
	Envelope messaging.Envelope
	Purged   bool
	PurgedAt *time.Time
}

// ReadChannel returns publications after since, oldest first (at most limit).
func (s *Store) ReadChannel(ctx context.Context, name string, since int64, limit int) ([]ChannelMessage, error) {
	return s.readChannel(ctx, name, since, limit, false)
}

// ReadLatestChannel returns the latest limit publications, oldest first.
func (s *Store) ReadLatestChannel(ctx context.Context, name string, limit int) ([]ChannelMessage, error) {
	return s.readChannel(ctx, name, 0, limit, true)
}

func (s *Store) readChannel(ctx context.Context, name string, since int64, limit int, latest bool) ([]ChannelMessage, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT p.seq, m.id, m.kind, m.channel, m.from_urn, m.to_urn, m.thread_id, m.in_reply_to,
 m.payload, m.content_type, m.metadata, m.created_at, m.delivered_at, m.consumed_at,
 (SELECT MAX(at) FROM message_purge_audit a WHERE a.table_name = 'messages' AND a.message_id = m.id)
 FROM channel_publications p JOIN messages m ON m.id = p.message_id
 WHERE p.name = ? AND p.seq > ? AND m.to_urn = ?
 AND (? = 0 OR p.seq IN (SELECT seq FROM channel_publications WHERE name = ? ORDER BY seq DESC LIMIT ?))
 ORDER BY p.seq LIMIT ?`), name, since, ChannelURN(name), boolInt(latest), name, limit, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ChannelMessage
	for rows.Next() {
		var msg ChannelMessage
		var purgedAt sql.NullString
		env, err := scanEnvelope(func(dest ...any) error {
			return rows.Scan(append(append([]any{&msg.Seq}, dest...), &purgedAt)...)
		})
		if err != nil {
			return nil, err
		}
		msg.Envelope = env
		if purgedAt.Valid {
			at, err := time.Parse(time.RFC3339Nano, purgedAt.String)
			if err != nil {
				return nil, err
			}
			msg.Purged, msg.PurgedAt = true, &at
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// AttachmentEnvelope returns a message with its staged flag (any routing state), for the channel
// slice's checks before AttachStagedToChannel.
func (s *Store) AttachmentEnvelope(ctx context.Context, id string) (messaging.Envelope, bool, error) {
	return attachmentEnvelope(ctx, s.c.Reader(), id)
}

func attachmentEnvelope(ctx context.Context, q gostore.Querier, id string) (messaging.Envelope, bool, error) {
	var staged int64
	env, err := scanEnvelope(q.QueryRowContext(ctx, q.Dialect().Rebind(`SELECT `+envelopeColumns+`, routing_staged FROM messages WHERE id = ?`), id).Scan, &staged)
	if errors.Is(err, sql.ErrNoRows) {
		return messaging.Envelope{}, false, messaging.ErrNotFound
	}
	return env, staged != 0, err
}

// ChannelAttach promotes a session's staged turn output into a channel. The channel slice has
// already validated the channel name, the session's route and kinds, and the publisher's
// authorization (old AttachChannelMessage); this is the storage half.
type ChannelAttach struct {
	MessageID, SessionID, Channel string
	Kind                          string            // the metadata kind the caller checked
	SetMetadata                   map[string]string // added to the message's metadata (launch_id, launch_display_name)
	Audit                         eventstore.Row    // the session.turn.routed outbox event
}

// AttachStagedToChannel runs the old attach transaction as the allowlisted joint
// "AttachChannelMessage" (class C rows plus the class B outbox event, until the S8 redesign): it
// re-reads the message, and if it is already a publication of this channel returns it unchanged
// (idempotent); otherwise it requires a staged, unexpired body from the session with the checked
// kind, readdresses it to the channel, records the publication (under LockMessageSeq, so channel
// seqs commit in order) and writes the audit event pending to the outbox.
func (s *Store) AttachStagedToChannel(ctx context.Context, a ChannelAttach, events *eventstore.Store) (messaging.Envelope, error) {
	sender := messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: a.SessionID}
	if a.MessageID == "" || a.SessionID == "" || a.Channel == "" || events == nil {
		return messaging.Envelope{}, ErrAttachInvalid
	}
	target := ChannelURN(a.Channel)
	var out messaging.Envelope
	err := s.all.Joint(ctx, "AttachChannelMessage", func(ctx context.Context, tx store.Tx) error {
		current, staged, err := attachmentEnvelope(ctx, tx, a.MessageID)
		if err != nil {
			return err
		}
		if current.From != sender {
			return ErrAttachInvalid
		}
		if !staged {
			var name string
			err := tx.QueryRowContext(ctx, rebind(tx, `SELECT name FROM channel_publications WHERE message_id = ?`), a.MessageID).Scan(&name)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAttachInvalid
			}
			if err != nil {
				return err
			}
			if name != a.Channel || current.To.URN() != target || current.ThreadID != a.SessionID {
				return ErrAttachInvalid
			}
			out = current
			return nil
		}
		if len(current.Payload) == 0 || !current.CreatedAt.After(s.now().Add(-RoutingStageRetention)) {
			return messaging.ErrNotFound
		}
		if current.Metadata["kind"] != a.Kind || current.Metadata["session_id"] != a.SessionID {
			return ErrAttachInvalid
		}
		meta := maps.Clone(current.Metadata)
		if meta == nil {
			meta = map[string]string{}
		}
		maps.Copy(meta, a.SetMetadata)
		raw, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		if err := LockMessageSeq(ctx, tx); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE messages SET to_urn = ?, channel = ?, thread_id = ?, metadata = ?, routing_staged = 0
 WHERE id = ? AND routing_staged = 1 AND payload IS NOT NULL AND from_urn = ?`), target, a.Channel, a.SessionID, string(raw), a.MessageID, sender.URN())
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err == nil {
				err = messaging.ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO channel_publications (name, message_id) VALUES (?, ?)`), a.Channel, a.MessageID); err != nil {
			return err
		}
		if _, _, err := events.InsertTx(ctx, tx, a.Audit, true); err != nil {
			return err
		}
		current.To, _ = messaging.ParseURN(target)
		current.Channel, current.ThreadID, current.Metadata = messaging.Channel(a.Channel), a.SessionID, meta
		out = current
		return nil
	})
	if err != nil {
		return messaging.Envelope{}, err
	}
	return out, nil
}
