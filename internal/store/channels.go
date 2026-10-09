package store

import (
	"context"
	"database/sql"
	"time"

	gomsg "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

var _ channels.Backend = (*Store)(nil)

// SetChannelAuthorization installs the observe/enforce policy for every
// publication path. Install once during composition; nil means observe mode.
func (s *Store) SetChannelAuthorization(check channels.Authorization) {
	s.channelAuthMu.Lock()
	s.channelAuth = check
	s.channelAuthMu.Unlock()
}

func (s *Store) authorizeChannelPublication(ctx context.Context, env gomsg.Envelope) error {
	s.channelAuthMu.RLock()
	check := s.channelAuth
	s.channelAuthMu.RUnlock()
	if check == nil {
		return nil
	}
	p, verified := identity.FromContext(ctx)
	if !verified {
		p.ID = env.From.URN()
	}
	return check(ctx, "publish", string(env.Channel), p, env.From)
}

func (ms *messagingStore) requireMailbox(ctx context.Context, id string) error {
	var publication bool
	if err := ms.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM channel_publications WHERE message_id = ?)`, id).Scan(&publication); err != nil {
		return err
	}
	if publication {
		return channels.ErrMailboxOperation
	}
	// A reply to a routed message is queued and delivered by the dispatcher; a
	// mailbox verb on it (cancel, consume, read, archive) would change its message
	// row and nothing else, leaving the reply to be injected anyway.
	var reply bool
	if err := ms.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM routing_replies WHERE reply_id = ?)`, id).Scan(&reply); err != nil {
		return err
	}
	if reply {
		return ErrRoutingReplyNotMailbox
	}
	return nil
}

func (s *Store) SendChannel(ctx context.Context, env gomsg.Envelope) (gomsg.Envelope, error) {
	publication, err := channels.NormalizePublication(&env)
	if err != nil || !publication {
		return gomsg.Envelope{}, channels.ErrInvalid
	}
	return s.MessagingStore().Send(ctx, env)
}

func (s *Store) ListChannelNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT name FROM channel_publications ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
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

func (s *Store) ChannelHighWater(ctx context.Context, name string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM channel_publications WHERE name = ?`, name).Scan(&seq)
	return seq, err
}

func (s *Store) ReadChannel(ctx context.Context, name string, since int64, limit int) ([]channels.Message, error) {
	return s.readChannel(ctx, name, since, limit, false)
}

func (s *Store) ReadLatestChannel(ctx context.Context, name string, limit int) ([]channels.Message, error) {
	return s.readChannel(ctx, name, 0, limit, true)
}

func (s *Store) readChannel(ctx context.Context, name string, since int64, limit int, latest bool) ([]channels.Message, error) {
	address, err := channels.ChannelAddress(name)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.seq,
		m.id, m.kind, m.channel, m.from_urn, m.to_urn, m.thread_id, m.in_reply_to,
		m.payload, m.content_type, m.metadata, m.created_at, m.delivered_at, m.consumed_at,
		(SELECT MAX(at) FROM message_purge_audit a WHERE a.table_name = 'messages' AND a.message_id = m.id)
		FROM channel_publications p JOIN messages m ON m.id = p.message_id
		WHERE p.name = ? AND p.seq > ? AND m.to_urn = ?
		AND (? = 0 OR p.seq IN (SELECT seq FROM channel_publications WHERE name = ? ORDER BY seq DESC LIMIT ?))
		ORDER BY p.seq LIMIT ?`, name, since, address.URN(), latest, name, limit, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []channels.Message
	for rows.Next() {
		var msg channels.Message
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
