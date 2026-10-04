package teamsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

// Ceilings supplies trusted host policy, independently of caller-authored teams.
// TrustResolver additionally decides admission of each pinned slot definition.
type Ceilings interface {
	FormationCeilings(context.Context, Principal) (FormationPolicy, error)
}
type FormationPolicy struct {
	Limits                                         mesh.Limits
	MaxSlots, MaxMembersPerSlot, MaxInitialMembers int
	AllowedPermissions                             []teams.Permission
}

func ConservativePolicy() FormationPolicy {
	return FormationPolicy{Limits: mesh.Limits{MaxDepth: 2, MaxChildren: 4, FanOut: 4, Budget: 1, Timeout: 5 * time.Minute, MaxRounds: 16, MaxStalls: 4}, MaxSlots: 4, MaxMembersPerSlot: 4, MaxInitialMembers: 4, AllowedPermissions: []teams.Permission{teams.MayMessage}}
}

func (s *Service) formationPolicy(ctx context.Context, p Principal, req FormRequest) (mesh.Limits, string, error) {
	if err := teams.Validate(req.Team); err != nil {
		return mesh.Limits{}, "", fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	launchTeam, err := validateLaunch(req)
	if err != nil {
		return mesh.Limits{}, "", err
	}
	policy, err := s.deps.Ceilings.FormationCeilings(ctx, p)
	if err != nil {
		return mesh.Limits{}, "", err
	}
	if err = policy.Limits.Validate(); err != nil {
		return mesh.Limits{}, "", fmt.Errorf("%w: host ceilings: %w", ErrUnavailable, err)
	}
	if policy.MaxSlots < 1 || policy.MaxMembersPerSlot < 1 || policy.MaxInitialMembers < 1 || policy.Limits.MaxRounds < 1 || policy.Limits.MaxStalls < 1 {
		return mesh.Limits{}, "", ErrUnavailable
	}
	if len(req.Team.Slots) > policy.MaxSlots || (req.Team.Authority.Mode != teams.Strict) {
		return mesh.Limits{}, "", ErrDenied
	}
	for _, slot := range req.Team.Slots {
		if slot.Max > policy.MaxMembersPerSlot || slot.Min > policy.MaxMembersPerSlot {
			return mesh.Limits{}, "", ErrDenied
		}
	}
	for _, g := range req.Team.Authority.Grants {
		if !slices.Contains(policy.AllowedPermissions, g.Verb) {
			return mesh.Limits{}, "", ErrDenied
		}
	}
	limits, err := teams.ResolveLimits(req.Team.Policy.Spawn, policy.Limits, policy.Limits)
	if err != nil {
		return mesh.Limits{}, "", err
	}
	limits, err = teams.ResolveLimits(req.Launch.Limits, limits, limits)
	if err != nil {
		return mesh.Limits{}, "", err
	}
	ownerSlot := req.Team.Phases[0].OwnerSlot
	if ownerSlot == "" {
		ownerSlot = req.Team.Routing.CoordinatorSlot
	}
	if ownerSlot == "" {
		return mesh.Limits{}, "", ErrInvalidRequest
	}
	total := 0
	ownerProvisioned := false
	ownerCount := 0
	ownerMax := 0
	for _, slot := range launchTeam.Slots {
		if slot.Max > policy.MaxMembersPerSlot || slot.Min > policy.MaxMembersPerSlot {
			return mesh.Limits{}, "", ErrDenied
		}
		n := slot.Min
		if count, ok := req.Launch.Counts[slot.Name]; ok {
			n = count
		}
		if n < slot.Min || n > slot.Max {
			return mesh.Limits{}, "", ErrInvalidRequest
		}
		total += n
		identities := slot.Identities
		if selected, ok := req.Launch.PoolIdentities[slot.Name]; ok {
			identities = selected
		}
		matches := slot.Resolution == teams.Durable && slot.Identity == p.ID && n > 0
		if slot.Resolution == teams.Pool {
			for i, id := range identities {
				if i < n && id == p.ID {
					matches = true
				}
			}
		}
		if matches {
			if slot.Name != ownerSlot {
				return mesh.Limits{}, "", ErrInvalidRequest
			}
			ownerProvisioned = true
		}
		if slot.Name == ownerSlot {
			ownerCount = n
			ownerMax = slot.Max
		}
		decision, err := s.deps.Trust.ResolveTrust(ctx, teams.Member{Actor: p.ID, Kind: p.Kind, Status: "active", Governance: teams.Owner, Slot: ownerSlot}, slot)
		if err != nil {
			return mesh.Limits{}, "", err
		}
		if decision != teams.TrustAllow {
			return mesh.Limits{}, "", ErrDenied
		}
	}
	if total == 0 || total > limits.FanOut || total > limits.MaxChildren {
		return mesh.Limits{}, "", ErrDenied
	}
	if !ownerProvisioned {
		total++
		ownerCount++
	}
	if ownerCount > ownerMax || ownerCount > policy.MaxMembersPerSlot || total > policy.MaxInitialMembers {
		return mesh.Limits{}, "", ErrDenied
	}
	return limits, ownerSlot, nil
}

// validateLaunch mirrors the launch request's structural validation before any
// definition write. The library still compiles and enforces the actual launch.
func validateLaunch(req FormRequest) (teams.Team, error) {
	if req.Launch.TeamID != "" && req.Launch.TeamID != req.Team.ID || req.Launch.Version != 0 && req.Launch.Version != req.Team.Version || req.Launch.Key != "" {
		return teams.Team{}, ErrInvalidRequest
	}
	encoded, err := json.Marshal(req.Team)
	if err != nil {
		return teams.Team{}, ErrInvalidRequest
	}
	var team teams.Team
	if err = json.Unmarshal(encoded, &team); err != nil {
		return teams.Team{}, ErrInvalidRequest
	}
	slots := map[string]int{}
	for i, slot := range team.Slots {
		slots[slot.Name] = i
	}
	for name := range req.Launch.Counts {
		if _, ok := slots[name]; !ok {
			return teams.Team{}, ErrInvalidRequest
		}
	}
	for name, ids := range req.Launch.PoolIdentities {
		i, ok := slots[name]
		if !ok || team.Slots[i].Resolution != teams.Pool {
			return teams.Team{}, ErrInvalidRequest
		}
		slot := team.Slots[i]
		seen := map[mesh.URN]bool{}
		for _, id := range ids {
			if !slices.Contains(slot.Identities, id) || seen[id] {
				return teams.Team{}, ErrInvalidRequest
			}
			seen[id] = true
		}
		if len(ids) < slot.Min {
			return teams.Team{}, ErrInvalidRequest
		}
		team.Slots[i].Identities = append([]mesh.URN(nil), ids...)
		team.Slots[i].Max = min(slot.Max, len(ids))
	}
	if _, err = teams.CompileTeam(team, team.Phases); err != nil {
		return teams.Team{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return team, nil
}
