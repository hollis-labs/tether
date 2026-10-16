package teamsvc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
)

// AdoptRequest names exactly what is adopted: one actor, the session it runs
// and the registry binding generation it runs under. A roster name is never
// enough; the host refuses anything its own records do not confirm.
type AdoptRequest struct {
	Key, RunID, Slot  string
	Actor             mesh.URN
	SessionID         string
	BindingGeneration uint64
}

// AdoptionAdmitter is the Ceilings extension that gates adoption by host
// policy (enabled, paused) before the host is asked to verify anything.
// Ceilings without it refuse every adoption.
type AdoptionAdmitter interface {
	AdmitAdoption(context.Context, Principal, teams.Team, teams.Slot) error
}

// delegateGrants is all a delegated coordinator may hold. may_delegate is the
// owner's alone: a delegate can message and assign, never re-delegate.
var delegateGrants = []teams.Permission{teams.MayMessage, teams.MayAssign}

// Adopt joins an already-running actor and its session to the run. Only the
// run owner may adopt, and adoption never widens anything: the member fills a
// seat the run's definition already declares for this actor, within that slot's
// cardinality, with the slot's own grants. The coordinator seat is held only as
// the owner's delegate (message and assign); the owner stays the run owner and
// removing the delegate does not end the run. Releasing any adopted member
// keeps its session running and bound.
func (s *Service) Adopt(ctx context.Context, req AdoptRequest) (Result, error) {
	return s.call(ctx, "member.adopt", req.Key, req, func(ctx context.Context, p Principal, scope Scope, _ Record) (Result, error) {
		if s.deps.Adopter == nil {
			return Result{}, ErrUnavailable
		}
		if req.Slot == "" || req.SessionID == "" || req.BindingGeneration == 0 || (mesh.Actor{URN: req.Actor, Kind: mesh.ActorAgent}).Validate() != nil {
			return Result{}, ErrInvalidRequest
		}
		t, owner, err := s.run(ctx, req.RunID, p)
		if err != nil {
			return Result{}, err
		}
		if owner.Governance != teams.Owner {
			return Result{}, ErrDenied
		}
		slot, ok := findSlot(t, req.Slot)
		if !ok {
			return Result{}, ErrInvalidRequest
		}
		// Fail closed: host ceilings that cannot say adoption is enabled do not
		// get it by default.
		admitter, ok := s.deps.Ceilings.(AdoptionAdmitter)
		if !ok {
			return Result{}, fmt.Errorf("%w: the host policy has no adoption gate", ErrDenied)
		}
		if err = admitter.AdmitAdoption(ctx, p, t, slot); err != nil {
			return Result{}, err
		}
		if slot.Name == t.Routing.CoordinatorSlot {
			if err = delegateOnly(t, slot); err != nil {
				return Result{}, err
			}
		}
		key := serviceKey(scope) + "-adopt"
		memberID := stableKey(key)
		if err = s.adoptRoom(ctx, req, slot, memberID); err != nil {
			return Result{}, err
		}
		member, err := s.deps.Adopter.Adopt(ctx, AdoptCommand{Key: key, MemberID: memberID, RunID: req.RunID, Slot: slot, Actor: req.Actor, SessionID: req.SessionID, BindingGeneration: req.BindingGeneration, Principal: p.ID, Limits: s.deps.Defaults})
		if err != nil {
			return Result{}, err
		}
		if member.ID != memberID || member.Actor != req.Actor || member.Slot != slot.Name || member.Governance != teams.MemberRole || member.SpawnCapable {
			// The host answered with something this call did not ask for.
			return Result{}, errors.Join(ErrConflict, s.deps.Provisioner.Release(ctx, req.RunID, member))
		}
		err = s.deps.Roster.Mutate(ctx, req.RunID, func(r *teams.Roster) error {
			for _, m := range r.Members {
				if m.ID == member.ID {
					return nil
				}
			}
			if err := roomIn(*r, slot, req.Actor, memberID); err != nil {
				return err
			}
			r.Members = append(r.Members, member)
			return nil
		})
		if err != nil {
			// Lost the seat to a concurrent adoption: let the member go. Its
			// session and binding are untouched by Release.
			return Result{}, errors.Join(err, s.deps.Provisioner.Release(ctx, req.RunID, member))
		}
		view := memberView(member)
		return Result{Member: &view}, nil
	})
}

func findSlot(t teams.Team, name string) (teams.Slot, bool) {
	for _, slot := range t.Slots {
		if slot.Name == name {
			return slot, true
		}
	}
	return teams.Slot{}, false
}

// delegateOnly refuses a coordinator seat whose grants exceed a delegate's.
func delegateOnly(t teams.Team, slot teams.Slot) error {
	for _, g := range t.Authority.Grants {
		if g.FromSlot == slot.Name && !slices.Contains(delegateGrants, g.Verb) {
			return fmt.Errorf("%w: a delegated coordinator may hold only may_message and may_assign, not %s", ErrDenied, g.Verb)
		}
	}
	return nil
}

func (s *Service) adoptRoom(ctx context.Context, req AdoptRequest, slot teams.Slot, memberID string) error {
	roster, err := s.deps.Roster.Snapshot(ctx, req.RunID)
	if err != nil {
		return err
	}
	return roomIn(roster, slot, req.Actor, memberID)
}

// roomIn counts every non-ended member of the slot, owner seat included, and
// refuses an actor that already holds a seat in this run.
func roomIn(r teams.Roster, slot teams.Slot, actor mesh.URN, memberID string) error {
	n := 0
	for _, m := range r.Members {
		if m.ID == memberID || !liveStatus(m.Status) {
			continue
		}
		if m.Actor == actor {
			return ErrConflict
		}
		if m.Slot == slot.Name {
			n++
		}
	}
	if n >= slot.Max {
		return fmt.Errorf("%w: slot %q is full", ErrConflict, slot.Name)
	}
	return nil
}

func liveStatus(status string) bool {
	switch status {
	case "active", "stopping", "releasing", "failing":
		return true
	default:
		return false
	}
}
