package teamruntime

import (
	"context"
	"errors"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// SessionActors answers which actor belongs to an already authenticated session.
// Identity admission and local-operator proof remain in Principals.
type SessionActors interface {
	SessionActor(context.Context, string) (mesh.URN, error)
}

// Principals is the only team authentication adapter. Under Enforce any verified
// principal kind is admitted. Under Off/Observe (dev mode) only the verified
// local operator is (credential plus accepted Unix socket, never a request
// field). Agents, services and remote callers are refused until Enforce, except
// a verified service the policy lists in Services (Torque's dispatcher): it is
// trusted as presented, with identity enforcement left where it is.
type Principals struct {
	Mode     identity.Mode
	Sessions SessionActors
	// Services reports whether a service principal id is policy-listed. Nil
	// lists none.
	Services func(id string) bool
}

func (p Principals) ResolvePrincipal(ctx context.Context) (teamsvc.Principal, error) {
	caller, verified := identity.FromContext(ctx)
	if !verified {
		return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
	}
	listedService := caller.Kind == "service" && p.Services != nil && p.Services(caller.ID)
	// Without enforcement nothing else checks scopes, so a listed service must
	// carry the teams scope itself.
	if listedService && p.Mode != identity.Enforce && !identity.ScopeGranted(caller.Scopes, "team.write") {
		return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
	}
	if p.Mode != identity.Enforce && caller.Kind != "operator" && !listedService {
		return teamsvc.Principal{}, teamsvc.ErrUnauthenticated
	}
	out := teamsvc.Principal{ID: mesh.URN(caller.ID), Verified: caller.Verified}
	switch caller.Kind {
	case "operator":
		if caller.ID != identity.OperatorID || caller.TokenID != "" || !identity.LocalConnection(ctx) {
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
