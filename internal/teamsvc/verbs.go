package teamsvc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

type FormRequest struct {
	Key    string
	Team   teams.Team
	Launch teams.LaunchRequest
}
type RunRequest struct{ Key, RunID string }
type AddMemberRequest struct {
	Key, RunID, Slot string
	Limits           mesh.Limits
}
type RemoveMemberRequest struct{ Key, RunID, MemberID string }
type CancelRequest struct {
	Key, RunID, MemberID string
	Cascade              bool
}
type MessageRequest struct {
	Key, RunID, Address, Body string
	Kind                      mesh.ActorKind
	History                   mesh.HistoryPolicy
}
type ReportResultRequest struct{ Key, RunID, DelegateKey, Body string }

func (s *Service) Form(ctx context.Context, req FormRequest) (Result, error) {
	return s.call(ctx, string(mesh.TeamForm), req.Key, req, func(ctx context.Context, p Principal, scope Scope, _ Record) (Result, error) {
		limits, slot, err := s.formationPolicy(ctx, p, req)
		if err != nil {
			return Result{}, err
		}
		if req.Launch.TeamID != "" && req.Launch.TeamID != req.Team.ID || req.Launch.Version != 0 && req.Launch.Version != req.Team.Version || req.Launch.Key != "" {
			return Result{}, ErrInvalidRequest
		}
		if err = s.deps.Definitions.PutDefinition(ctx, req.Team); err != nil {
			return Result{}, err
		}
		launch := req.Launch
		launch.TeamID, launch.Version, launch.Key, launch.Limits = req.Team.ID, req.Team.Version, serviceKey(scope)+"-launch", limits
		launcher := *s.launcher
		launcher.Defaults = limits
		run, err := launcher.Launch(ctx, launch)
		if err != nil {
			return Result{}, err
		}
		err = s.deps.Roster.Mutate(ctx, run.ID, func(r *teams.Roster) error {
			for i, m := range r.Members {
				if m.Actor == p.ID {
					if m.Kind != p.Kind || m.Status != "active" || m.Slot != slot {
						return ErrConflict
					}
					r.Members[i].Governance = teams.Owner
					return nil
				}
			}
			// A person/service owner is a governance participant, not a provisioned
			// worker. It acquires no session, spawn capability or provider resources.
			r.Members = append(r.Members, teams.Member{ID: serviceKey(scope) + "-owner", Slot: slot, Actor: p.ID, Kind: p.Kind, Status: "active", Governance: teams.Owner, JoinedAt: s.deps.Clock.Now()})
			return nil
		})
		return Result{Run: &run}, err
	})
}

func (s *Service) AddMember(ctx context.Context, req AddMemberRequest) (Result, error) {
	return s.call(ctx, string(mesh.MemberAdd), req.Key, req, func(ctx context.Context, p Principal, scope Scope, _ Record) (Result, error) {
		t, _, err := s.run(ctx, req.RunID, p)
		if err != nil {
			return Result{}, err
		}
		member, err := teams.Spawn(ctx, t, s.deps.Roster, s.deps.Provisioner, s.deps.Trust, s.deps.Approvals, req.RunID, p.ID, req.Slot, serviceKey(scope), req.Limits, s.deps.Defaults)
		view := memberView(member)
		return Result{Member: &view}, err
	})
}

func (s *Service) RemoveMember(ctx context.Context, req RemoveMemberRequest) (Result, error) {
	return s.terminate(ctx, string(mesh.MemberRemove), req.Key, req.RunID, req, req.MemberID, false, true)
}
func (s *Service) Cancel(ctx context.Context, req CancelRequest) (Result, error) {
	return s.terminate(ctx, string(mesh.Cancel), req.Key, req.RunID, req, req.MemberID, req.Cascade, false)
}

func (s *Service) terminate(ctx context.Context, verb, key, runID string, req any, memberID string, cascade, remove bool) (Result, error) {
	return s.call(ctx, verb, key, req, func(ctx context.Context, p Principal, scope Scope, record Record) (Result, error) {
		var plan terminationPlan
		if len(record.Plan) != 0 {
			if err := json.Unmarshal(record.Plan, &plan); err != nil {
				return Result{}, err
			}
			if plan.Actor.Actor != p.ID || plan.Actor.Kind != p.Kind {
				return Result{}, ErrConflict
			}
			// If reservation already committed, finish only its retained targets.
			// Otherwise run the library gate again against current membership.
			_, err := (selectedRoster{RosterStore: s.deps.Roster, plan: plan}).Snapshot(ctx, runID)
			if err == nil {
				return Result{}, teams.ReconcileMembers(ctx, selectedRoster{RosterStore: s.deps.Roster, plan: plan}, s.deps.Provisioner, runID)
			}
			if !errors.Is(err, ErrConflict) {
				return Result{}, err
			}
		}
		t, actor, err := s.run(ctx, runID, p)
		if err != nil {
			return Result{}, err
		}
		snapshot, err := s.deps.Roster.Snapshot(ctx, runID)
		if err != nil {
			return Result{}, err
		}
		targets, err := teams.Cascade(snapshot, memberID, true)
		if err != nil {
			return Result{}, err
		}
		if !cascade {
			targets = targets[len(targets)-1:]
		}
		if len(record.Plan) == 0 {
			probe := authorizationProbe{RosterStore: s.deps.Roster, roster: snapshot}
			if remove {
				err = teams.RemoveMember(ctx, t, probe, s.deps.Provisioner, runID, p.ID, memberID)
			} else {
				err = teams.CancelMembers(ctx, t, probe, s.deps.Provisioner, runID, p.ID, memberID, cascade)
			}
			if !errors.Is(err, errAuthorized) {
				return Result{}, err
			}
			plan = terminationPlan{Actor: actor, Members: targets}
			encoded, err := json.Marshal(plan)
			if err != nil {
				return Result{}, err
			}
			if err = s.deps.Calls.SetPlan(ctx, scope, encoded); err != nil {
				return Result{}, err
			}
		}
		// Retrying a plan may never cancel a new child or replacement session.
		if !sameMembers(plan.Members, targets) {
			return Result{}, ErrConflict
		}
		if remove {
			err = teams.RemoveMember(ctx, t, boundedTermination{RosterStore: s.deps.Roster, members: plan.Members, root: memberID, cascade: false}, s.deps.Provisioner, runID, p.ID, memberID)
		} else {
			err = teams.CancelMembers(ctx, t, boundedTermination{RosterStore: s.deps.Roster, members: plan.Members, root: memberID, cascade: cascade}, s.deps.Provisioner, runID, p.ID, memberID, cascade)
		}
		return Result{}, err
	})
}
func sameMembers(a, b []teams.Member) bool {
	if len(a) != len(b) {
		return false
	}
	for i, m := range a {
		if m.ID != b[i].ID || m.Actor != b[i].Actor || m.SessionID != b[i].SessionID {
			return false
		}
	}
	return true
}

// boundedTermination rechecks the retained target set in the library's own
// reservation transaction, closing the plan-to-reservation membership race.
type boundedTermination struct {
	teams.RosterStore
	members []teams.Member
	root    string
	cascade bool
}

func (b boundedTermination) Mutate(ctx context.Context, run string, fn func(*teams.Roster) error) error {
	return b.RosterStore.Mutate(ctx, run, func(r *teams.Roster) error {
		targets, err := teams.Cascade(*r, b.root, true)
		if err != nil {
			return err
		}
		if !b.cascade {
			targets = targets[len(targets)-1:]
		}
		if !sameMembers(b.members, targets) {
			return ErrConflict
		}
		return fn(r)
	})
}

// selectedRoster limits trusted library recovery to the already-authorized
// termination intent. It cannot recover another operation's provisioning.
type selectedRoster struct {
	teams.RosterStore
	plan terminationPlan
}

func (r selectedRoster) Snapshot(ctx context.Context, run string) (teams.Roster, error) {
	roster, err := r.RosterStore.Snapshot(ctx, run)
	if err != nil {
		return teams.Roster{}, err
	}
	var members []teams.Member
	for _, wanted := range r.plan.Members {
		for _, m := range roster.Members {
			if m.ID == wanted.ID {
				if m.Actor != wanted.Actor || m.SessionID != wanted.SessionID || m.Status == "active" || m.Status == "provisioning" {
					return teams.Roster{}, ErrConflict
				}
				members = append(members, m)
			}
		}
	}
	if len(members) != len(r.plan.Members) {
		return teams.Roster{}, ErrNotFound
	}
	roster.Members = members
	return roster, nil
}

func (r selectedRoster) Mutate(ctx context.Context, run string, fn func(*teams.Roster) error) error {
	return r.RosterStore.Mutate(ctx, run, func(roster *teams.Roster) error {
		for _, wanted := range r.plan.Members {
			found := false
			for _, m := range roster.Members {
				if m.ID == wanted.ID {
					if m.Actor != wanted.Actor || m.SessionID != wanted.SessionID || m.Status == "active" || m.Status == "provisioning" {
						return ErrConflict
					}
					found = true
				}
			}
			if !found {
				return ErrNotFound
			}
		}
		return fn(roster)
	})
}

func (s *Service) Address(ctx context.Context, req MessageRequest) (Result, error) {
	return s.message(ctx, mesh.MessageAddress, req)
}
func (s *Service) Assign(ctx context.Context, req MessageRequest) (Result, error) {
	return s.message(ctx, mesh.Assign, req)
}
func (s *Service) Delegate(ctx context.Context, req MessageRequest) (Result, error) {
	return s.message(ctx, mesh.Delegate, req)
}

func (s *Service) message(ctx context.Context, verb mesh.Verb, req MessageRequest) (Result, error) {
	return s.call(ctx, string(verb), req.Key, req, func(ctx context.Context, p Principal, scope Scope, record Record) (Result, error) {
		t, _, err := s.run(ctx, req.RunID, p)
		if err != nil {
			return Result{}, err
		}
		if req.Body == "" {
			return Result{}, ErrInvalidRequest
		}
		address := teams.AddressRequest{RunID: req.RunID, Actor: p.ID, Address: req.Address, Body: req.Body, Verb: verb, Kind: req.Kind, History: req.History}
		var route teams.Route
		if len(record.Plan) == 0 {
			route, err = s.router.Resolve(ctx, t, address)
			if err != nil {
				return Result{}, err
			}
			plan, err := json.Marshal(route)
			if err != nil {
				return Result{}, err
			}
			if err = s.deps.Calls.SetPlan(ctx, scope, plan); err != nil {
				return Result{}, err
			}
		} else if err = json.Unmarshal(record.Plan, &route); err != nil {
			return Result{}, err
		}
		key := serviceKey(scope)
		if err = s.router.SendResolved(ctx, t, address, route, key); err != nil {
			return Result{}, err
		}
		result := Result{}
		for _, m := range route.Recipients {
			result.Recipients = append(result.Recipients, memberView(m))
			result.DeliveryKeys = append(result.DeliveryKeys, deliveryKey(req.RunID, key, m.ID))
		}
		return result, nil
	})
}

// deliveryKey mirrors the library's published recipient-key derivation.
// Host senders retain this exact key for assignee-only result acknowledgement.
func deliveryKey(run, key, member string) string {
	return stableKey(run + "\x00" + key + "\x00" + member)
}

func (s *Service) ReportResult(ctx context.Context, req ReportResultRequest) (Result, error) {
	return s.call(ctx, string(mesh.ReportResult), req.Key, req, func(ctx context.Context, p Principal, _ Scope, _ Record) (Result, error) {
		t, _, err := s.run(ctx, req.RunID, p)
		if err != nil {
			return Result{}, err
		}
		if req.DelegateKey == "" || req.Body == "" {
			return Result{}, ErrInvalidRequest
		}
		err = s.router.ReplyResult(ctx, t, req.RunID, req.DelegateKey, p.ID, req.Body)
		return Result{}, err
	})
}

// terminationPlan retains the authorized actor and entire roster. The guarded
// store rechecks administration atomically before the first EndRun mutation;
// recovery permits only the same identities/sessions and cannot stop newcomers.
type terminationPlan struct {
	Actor   teams.Member
	Members []teams.Member
}
type guardedRoster struct {
	teams.RosterStore
	team        teams.Team
	plan        terminationPlan
	provisioner teams.MemberProvisioner
}

func (g guardedRoster) Mutate(ctx context.Context, run string, fn func(*teams.Roster) error) error {
	return g.RosterStore.Mutate(ctx, run, func(r *teams.Roster) error {
		for _, m := range r.Members {
			if m.Status != "active" && m.Status != "provisioning" && m.Status != "stopping" && m.Status != "releasing" && m.Status != "failing" {
				continue
			}
			found := false
			for _, old := range g.plan.Members {
				if old.ID == m.ID && old.Actor == m.Actor && old.SessionID == m.SessionID && old.Slot == m.Slot {
					found = true
					break
				}
			}
			if !found {
				return ErrConflict
			}

		}
		from := g.plan.Actor
		liveActor := false
		needsLiveAuthority := false
		for _, old := range g.plan.Members {
			found := false
			for _, m := range r.Members {
				if m.ID == old.ID {
					found = true
					if m.Status == "active" || m.Status == "provisioning" {
						needsLiveAuthority = true
					}
					if m.Actor == from.Actor && m.Kind == from.Kind && m.Status == "active" {
						from = m
						liveActor = true
					}
				}
			}
			if !found {
				return ErrNotFound
			}
		}
		if needsLiveAuthority && !liveActor {
			return ErrNotFound
		}
		if err := admin(ctx, g.team, g.RosterStore, g.provisioner, from, r.Members); err != nil {
			return err
		}
		return fn(r)
	})
}

// authorizationProbe runs the library's roster-administration transaction
// against a detached roster and aborts before committing or provisioning. This
// preserves its owner/admin rules without duplicating them in the service.
var errAuthorized = errors.New("teamsvc: authorization probe complete")

type authorizationProbe struct {
	teams.RosterStore
	roster teams.Roster
}

func (p authorizationProbe) Mutate(_ context.Context, _ string, fn func(*teams.Roster) error) error {
	encoded, err := json.Marshal(p.roster)
	if err != nil {
		return err
	}
	var roster teams.Roster
	if err = json.Unmarshal(encoded, &roster); err != nil {
		return err
	}
	if err = fn(&roster); err != nil {
		return err
	}
	return errAuthorized
}
func admin(ctx context.Context, t teams.Team, store teams.RosterStore, provisioner teams.MemberProvisioner, from teams.Member, members []teams.Member) error {
	roster := teams.Roster{Members: append([]teams.Member(nil), members...)}
	found := false
	for i, m := range roster.Members {
		if m.ID == from.ID {
			roster.Members[i] = from
			found = true
		}
	}
	if !found {
		roster.Members = append(roster.Members, from)
	}
	probe := authorizationProbe{RosterStore: store, roster: roster}
	for _, target := range roster.Members {
		err := teams.CancelMembers(ctx, t, probe, provisioner, "probe", from.Actor, target.ID, false)
		if !errors.Is(err, errAuthorized) {
			return err
		}
	}
	return nil
}
func (s *Service) Dissolve(ctx context.Context, req RunRequest) (Result, error) {
	return s.call(ctx, string(mesh.TeamDissolve), req.Key, req, func(ctx context.Context, p Principal, scope Scope, record Record) (Result, error) {
		var plan terminationPlan
		var t teams.Team
		if len(record.Plan) == 0 {
			team, actor, err := s.run(ctx, req.RunID, p)
			if err != nil {
				return Result{}, err
			}
			t = team
			roster, err := s.deps.Roster.Snapshot(ctx, req.RunID)
			if err != nil {
				return Result{}, err
			}
			plan = terminationPlan{Actor: actor, Members: roster.Members}
			if err = admin(ctx, t, s.deps.Roster, s.deps.Provisioner, actor, roster.Members); err != nil {
				return Result{}, err
			}
			payload, err := json.Marshal(plan)
			if err != nil {
				return Result{}, err
			}
			if err = s.deps.Calls.SetPlan(ctx, scope, payload); err != nil {
				return Result{}, err
			}
		} else {
			if err := json.Unmarshal(record.Plan, &plan); err != nil {
				return Result{}, err
			}
			if plan.Actor.Actor != p.ID || plan.Actor.Kind != p.Kind {
				return Result{}, ErrConflict
			}
			run, err := s.deps.Runs.GetRun(ctx, req.RunID)
			if err != nil {
				return Result{}, err
			}
			t, err = s.deps.Definitions.GetDefinition(ctx, run.TeamID, run.TeamVersion)
			if err != nil {
				return Result{}, err
			}
			if t.Authority.Mode != teams.Strict {
				return Result{}, ErrDenied
			}
		}
		err := teams.EndRun(ctx, req.RunID, guardedRoster{RosterStore: s.deps.Roster, team: t, plan: plan, provisioner: s.deps.Provisioner}, s.deps.Provisioner, s.deps.Routing)
		return Result{}, err
	})
}

func stableKey(text string) string {
	digest := sha256.Sum256([]byte(text))
	return fmt.Sprintf("team-%x", digest[:16])
}
