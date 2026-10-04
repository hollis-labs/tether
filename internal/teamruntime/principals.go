package teamruntime

import (
	"context"
	"errors"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// SessionActors answers which actor belongs to an already authenticated session.
// Identity admission and local-operator proof remain in Principals.
type SessionActors interface {
	SessionActor(context.Context, string) (mesh.URN, error)
}

// Principals is the only team authentication adapter. Observe/off never grants
// team authority, even if attribution middleware carried a valid principal.
// Operator authority additionally requires an accepted Unix socket connection.
type Principals struct {
	Mode     identity.Mode
	Sessions SessionActors
}

func (p Principals) ResolvePrincipal(ctx context.Context) (teamsvc.Principal, error) {
	if p.Mode != identity.Enforce {
		return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
	}
	caller, verified := identity.FromContext(ctx)
	if !verified {
		return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
	}
	out := teamsvc.Principal{ID: mesh.URN(caller.ID), Verified: true}
	switch caller.Kind {
	case "operator":
		if caller.ID != identity.OperatorID || !identity.LocalConnection(ctx) {
			return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
		}
		out.Kind = mesh.ActorUser
		out.LocalOperator = true
	case "session":
		if caller.SessionID == "" || p.Sessions == nil {
			return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
		}
		actor, err := p.Sessions.SessionActor(ctx, caller.SessionID)
		if err != nil {
			if errors.Is(err, teams.ErrNotFound) || errors.Is(err, teams.ErrDenied) {
				return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
			}
			return teamsvc.Principal{}, err
		}
		out.ID = actor
		out.Kind = mesh.ActorAgent
	case "service":
		out.Kind = mesh.ActorService
	case "interactive":
		if caller.ID == identity.OperatorID {
			return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
		}
		out.Kind = mesh.ActorUser
	default:
		return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
	}
	if (mesh.Actor{URN: out.ID, Kind: out.Kind}).Validate() != nil {
		return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
	}
	return out, nil
}
