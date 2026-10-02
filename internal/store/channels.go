package store

import (
	"context"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

var _ channels.Backend = (*Store)(nil)

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
	address, err := channels.ChannelAddress(name)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.seq,
		m.id, m.kind, m.channel, m.from_urn, m.to_urn, m.thread_id, m.in_reply_to,
		m.payload, m.content_type, m.metadata, m.created_at, m.delivered_at, m.consumed_at
		FROM channel_publications p JOIN messages m ON m.id = p.message_id
		WHERE p.name = ? AND p.seq > ? AND m.to_urn = ?
		ORDER BY p.seq LIMIT ?`, name, since, address.URN(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []channels.Message
	for rows.Next() {
		var msg channels.Message
		env, err := scanEnvelope(func(dest ...any) error {
			return rows.Scan(append([]any{&msg.Seq}, dest...)...)
		})
		if err != nil {
			return nil, err
		}
		msg.Envelope = env
		out = append(out, msg)
	}
	return out, rows.Err()
}
