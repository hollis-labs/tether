package teamruntime

import (
	"context"
	"database/sql"
	"errors"

	messageDelivery "github.com/hollis-labs/go-messaging/delivery"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
)

type MessageService interface {
	QueueTeamDelivery(context.Context, teams.Delivery) error
}
type Messenger struct {
	receipts
	service MessageService
}

func NewMessenger(db *sql.DB, s *teamstore.Store, service MessageService) (*Messenger, error) {
	if db == nil || s == nil || service == nil {
		return nil, errors.New("team messenger: store and message service required")
	}
	return &Messenger{receipts{db, s}, service}, nil
}
func (m *Messenger) Deliver(ctx context.Context, d teams.Delivery) error {
	unlock := m.lock("delivery", d.IdempotencyKey)
	defer unlock()
	saved, err := m.reserve(ctx, "delivery", d.IdempotencyKey, d)
	if err != nil {
		return err
	}
	if saved.state == "failed" {
		if string(saved.payload) == "invalid" {
			return teamhost.ErrInvalidRequest
		}
		return teamhost.ErrSessionGone
	}
	if saved.state == "done" {
		return nil
	}
	if err = m.service.QueueTeamDelivery(ctx, d); err != nil {
		// The receipt already proved an identical request. A transport digest
		// conflict cannot be repaired by replaying that request.
		if errors.Is(err, teams.ErrConflict) || errors.Is(err, messageDelivery.ErrDigestConflict) || errors.Is(err, messageDelivery.ErrInvalidArgument) {
			err = errors.Join(teamhost.ErrPermanent, err)
		}
		if errors.Is(err, teamhost.ErrSessionGone) || errors.Is(err, teamhost.ErrInvalidRequest) {
			code := "gone"
			if errors.Is(err, teamhost.ErrInvalidRequest) {
				code = "invalid"
			}
			_, persistErr := m.db.ExecContext(ctx, `UPDATE team_port_intents SET state='failed',payload=? WHERE port_kind='delivery' AND intent_key=?`, []byte(code), d.IdempotencyKey)
			return errors.Join(err, persistErr)
		}
		return err
	}
	return m.complete(ctx, "delivery", d.IdempotencyKey, d.IdempotencyKey)
}

// Channels derives names only; publications create channels through the normal
// implicit channel mechanism. No constructor, Name or enrollment publishes.
type Channels struct{}

func (Channels) Name(run string) (string, error) { return teamstore.ChannelName(run) }
