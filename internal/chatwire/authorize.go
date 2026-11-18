package chatwire

import (
	"context"
	"errors"
	"fmt"
	"slices"

	chat "github.com/hollis-labs/libs/agent/chat"

	"github.com/hollis-labs/tether/internal/identity"
)

// ErrForbidden is Authorize's refusal.
var ErrForbidden = errors.New("chatwire: not allowed")

// Authorize is the chat handler's chat.Authorize from Tether's caller identity. Every conversation this daemon serves
// is owned by the operator (identity.OperatorID), so the question is whether the caller acts for the operator:
//
//   - the operator's own credential, or any principal holding "*", may do everything;
//   - a local caller with no verified principal (identity mode off, or an unauthenticated local socket client) is the
//     operator's custody, as on every other local route;
//   - any other verified principal (a paired device, an issued token, a session credential) needs the scope the
//     permission maps to: read for read, operate for control and takeover;
//   - admin is reserved for the host and refused.
//
// A remote caller without a verified principal is refused; the remote listener admits none anyway.
func Authorize(ctx context.Context, owner string, p chat.Permission) error {
	if owner != identity.OperatorID {
		return fmt.Errorf("%w: the conversation is not the operator's", ErrForbidden)
	}
	if p == chat.PermAdmin {
		return fmt.Errorf("%w: admin is reserved", ErrForbidden)
	}
	pr, ok := identity.FromContext(ctx)
	if !ok || !pr.Verified {
		if identity.IsRemote(ctx) {
			return fmt.Errorf("%w: unverified remote caller", ErrForbidden)
		}
		return nil
	}
	if pr.ID == identity.OperatorID || slices.Contains(pr.Scopes, "*") {
		return nil
	}
	scope := identity.ScopeRead
	if p != chat.PermRead {
		scope = identity.ScopeOperate
	}
	if identity.ScopeGranted(pr.Scopes, scope) || identity.HasDeviceScope(pr, scope) {
		return nil
	}
	return fmt.Errorf("%w: %s needs scope %s", ErrForbidden, p, scope)
}
