package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// An agent instance address, msg://agent/<authority>/<logical agent>/<session id>, is a
// readable spelling of msg://session/<authority>/<session id> that also names the logical
// agent the session belongs to (CW-20261023-0018, Portfolio Architect option C(a)). It is
// derived, never registered: no registry row and no runtime binding per instance. It never
// falls back: an ended or unknown session does not redirect to the agent's bare mailbox,
// its home session or its successor.

// IsInstanceAddress reports whether to is an agent instance address.
func IsInstanceAddress(to messaging.Address) bool {
	return to.Kind == messaging.KindAgent && to.SubID != ""
}

// instanceAuthority is the one authority an instance address may carry: the hub's own,
// the same one every logical-agent binding target uses. A mailbox is keyed by the exact
// address string, so another authority would be a different, unrelated mailbox.
func instanceAuthority() string {
	a, err := messaging.ParseURN(registry.LogicalAgentBindingTarget("x"))
	if err != nil {
		return ""
	}
	return a.Authority
}

// ResolveInstanceSession answers which running session an agent instance address names.
// api.ErrInstanceUnknown: no such session, or it belongs to another logical agent (the
// two are not told apart). *api.InstanceEndedError: it ended, with its resumed successor
// when known. api.ErrBoundSessionNotRunning: it exists and has not ended but has no
// running runtime yet (still launching): stored mail is woken once it runs.
func (s *Service) ResolveInstanceSession(ctx context.Context, to messaging.Address) (string, error) {
	return resolveInstanceSession(ctx, s.Store, s.runtimeSeam(), to)
}

func resolveInstanceSession(ctx context.Context, st *store.Store, rt wakeRuntime, to messaging.Address) (string, error) {
	if !IsInstanceAddress(to) || st == nil {
		return "", api.ErrInstanceUnknown
	}
	row, err := st.GetSessionContext(ctx, to.SubID)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			return "", api.ErrInstanceUnknown
		}
		return "", err
	}
	if row.LogicalAgentID != to.ID || to.Authority != instanceAuthority() {
		return "", api.ErrInstanceUnknown
	}
	if _, ok := rt.health(row.ID); ok {
		return row.ID, nil
	}
	if session.State(row.State).Terminal() {
		succ, serr := st.SuccessorSessionID(ctx, row.ID)
		if serr != nil {
			return "", fmt.Errorf("resolve successor of %s: %w", row.ID, serr)
		}
		return "", &api.InstanceEndedError{SessionID: row.ID, SupersededBy: succ}
	}
	return "", api.ErrBoundSessionNotRunning
}
