package store

import (
	"context"

	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"
)

// Historical ordinal must follow committed claim sequence, not timestamps:
// the library explicitly permits nonmonotonic transition times. One read-only
// SQLite snapshot subtracts subsequent claims from the durable attempt count,
// including pre-log attempts. This does not mutate library-owned tables.
func (s *Store) messageTransitionAttempt(ctx context.Context, tr delivery.Transition) (int, int64, error) {
	var number int
	err := s.db.QueryRowContext(ctx, `SELECT attempt_count - (SELECT count(*) FROM messaging_transitions t WHERE t.delivery_id=d.id AND t.reason=? AND t.id>?) FROM messaging_deliveries d WHERE d.id=?`, delivery.TransitionClaimed, tr.ID, tr.DeliveryID).Scan(&number)
	if err != nil {
		return 0, 0, err
	}
	if tr.AttemptID == "" {
		return number, 0, nil
	}
	attempts, err := s.DeliveryStore().Attempts(ctx, tr.DeliveryID)
	if err != nil {
		return 0, 0, err
	}
	for _, attempt := range attempts {
		if attempt.ID == tr.AttemptID {
			return number, attempt.BindingGeneration, nil
		}
	}
	return 0, 0, delivery.ErrNotFound
}
