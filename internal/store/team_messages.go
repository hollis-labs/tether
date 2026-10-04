package store

import (
	"context"
	"errors"
	"fmt"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

// SendKeyedMessage accepts a mailbox message through the existing delivery core
// with sender-scoped idempotency. If content persistence failed after Enqueue,
// retry repairs that content row using the SAME message/delivery identifiers.
// Ordinary Send is unchanged. Team routing never uses this for a publication.
func (s *Store) SendKeyedMessage(ctx context.Context, key string, env messaging.Envelope) (messaging.Envelope, error) {
	if key == "" {
		return messaging.Envelope{}, fmt.Errorf("%w: keyed message key required", delivery.ErrInvalidArgument)
	}
	publication, err := channels.NormalizePublication(&env)
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("%w: %w", delivery.ErrInvalidArgument, err)
	}
	if publication {
		return messaging.Envelope{}, fmt.Errorf("%w: keyed message mailbox only", delivery.ErrInvalidArgument)
	}
	if env.DeliveredAt != nil || env.ConsumedAt != nil {
		return messaging.Envelope{}, messaging.ErrPresetLifecycle
	}
	d := s.MessagingStore().(*deliveryBackedStore)
	request, err := delivery.EnvelopeEnqueueRequest(env)
	if err != nil {
		return messaging.Envelope{}, err
	}
	request.IdempotencyKey = "team-port:" + key
	retained, err := d.delivery.Enqueue(ctx, request)
	if err != nil {
		return messaging.Envelope{}, err
	}
	if len(retained.Deliveries) != 1 {
		return messaging.Envelope{}, fmt.Errorf("%w: keyed message expected one recipient", delivery.ErrInvalidArgument)
	}
	if retained.Duplicate {
		saved, err := d.Get(ctx, string(retained.Message.ID))
		if err == nil {
			return saved, nil
		}
		if !errors.Is(err, messaging.ErrNotFound) {
			return messaging.Envelope{}, err
		}
	}
	env.ID = string(retained.Message.ID)
	env.CreatedAt = retained.Message.CreatedAt
	return d.sendWithID(ctx, env, string(retained.Deliveries[0].ID))
}
