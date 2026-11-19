// Package terminaltarget resolves terminal targets from Tether's mapping table.
// It has no chat host dependency: resolution runs under the host's lease lock.
package terminaltarget

import (
	"context"
	"fmt"

	"github.com/hollis-labs/tether/internal/conversation"
)

// Target is the current mapped session and whether its PTY lives on this daemon.
type Target struct {
	conversation.Target
	Terminal bool
}

// Resolver is the terminal attachments' TargetResolver: a conversation or session ID to the current chat host
// binding and exact PTY session. It reads the Tether store and the sessions' in-process PTY table only.
type Resolver struct {
	Mapping   conversation.Resolver
	Sessions  interface{ HasTerminal(sessionID string) bool }
	Authorize func(ctx context.Context, owner string) error
}

// Resolve reads the mapping without consulting the chat host. Unauthorized targets
// are hidden as conversation.ErrNotFound; superseded sessions are conversation.ErrStale.
func (r *Resolver) Resolve(ctx context.Context, target string) (Target, error) {
	t, err := r.Mapping.Resolve(ctx, target)
	if err != nil {
		return Target{}, err
	}
	if err := r.Authorize(ctx, t.Owner); err != nil {
		return Target{}, fmt.Errorf("%w: %w", conversation.ErrNotFound, err)
	}
	return Target{Target: t, Terminal: t.WorkerID == "" && t.SessionID != "" && r.Sessions.HasTerminal(t.SessionID)}, nil
}
