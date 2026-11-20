package store

import (
	"context"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

// DeliveryMarker records a successful recipient handoff. Inspection reads must
// not call it. The database journals only the first transition, atomically.
type DeliveryMarker interface {
	MarkDelivered(context.Context, string, messaging.Address, string, string) error
}

func (ms *messagingStore) MarkDelivered(ctx context.Context, id string, recipient messaging.Address, path, sessionID string) error {
	var to string
	if err := ms.db.QueryRowContext(ctx, `SELECT to_urn FROM messages WHERE id=? AND routing_staged=0`, id).Scan(&to); err != nil {
		return err
	}
	mine, err := ms.sameMailbox(ctx, to, recipient)
	if err != nil {
		return err
	}
	if !mine {
		return ErrWrongRecipient
	}
	_, err = ms.db.ExecContext(ctx, `UPDATE messages SET delivered_at=COALESCE(read_at,?), delivery_path=?, delivery_session_id=? WHERE id=? AND delivered_at IS NULL AND routing_staged=0`, time.Now().UTC().Format(time.RFC3339Nano), path, sessionID, id)
	return err
}
