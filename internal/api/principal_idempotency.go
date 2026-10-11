package api

import (
	"context"
	"errors"

	"github.com/hollis-labs/tether/internal/identity"
)

// idempotencyPrincipal is the principal an idempotency key is scoped to
// (CW-20261010-0263). The key itself is stored as the client sent it; the
// scope is the second half of the store's primary key, so one caller's keys
// can neither collide with nor replay another's.
//
// A remote key binds to the actual verified device, never a label,
// caller-selected from/as, address or header. A local caller binds to its
// verified principal. A local caller with no verified principal (observe mode
// admits them) yields "", which the service scopes to the shared anonymous
// space, exactly as before.
func idempotencyPrincipal(ctx context.Context) (string, error) {
	p, ok := identity.FromContext(ctx)
	if !identity.IsRemote(ctx) {
		if !ok {
			return "", nil
		}
		return p.ID, nil
	}
	if !ok || p.Kind != "device" || p.ID == "" || p.ID == identity.OperatorID {
		return "", errors.New("verified device required")
	}
	return p.ID, nil
}
