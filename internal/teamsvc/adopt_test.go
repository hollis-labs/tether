package teamsvc_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

const (
	operatorURN mesh.URN = "msg://user/local/operator"
	delegateURN mesh.URN = "msg://agent/fleet/lead"
	workerOne   mesh.URN = "msg://agent/fleet/one"
	workerTwo   mesh.URN = "msg://agent/fleet/two"
	strangerURN mesh.URN = "msg://agent/fleet/stranger"
)

var adoptPin = mesh.DefinitionRef{ID: "claude-agent", Revision: "r1", Digest: "sha256:aaaa"}

// refusingPorts are the ports a team host owns for sessions it provisions. An
// adopted member must never reach any of them, so every call fails the test.
type refusingPorts struct{ t *testing.T }

func (p refusingPorts) Launch(context.Context, teamhost.SessionRequest) (string, error) {
	p.t.Error("adoption launched a session")
	return "", errors.New("launch")
}
func (p refusingPorts) Stop(context.Context, string) error {
	p.t.Error("an adopted session was stopped")
	return errors.New("stop")
}
func (p refusingPorts) Ensure(context.Context, teamhost.EnrollmentRequest) (teamhost.Enrollment, error) {
	p.t.Error("adoption enrolled")
	return teamhost.Enrollment{}, errors.New("ensure")
}
func (p refusingPorts) AcquireBinding(context.Context, string, mesh.URN) error {
	p.t.Error("adoption leased a binding")
	return errors.New("bind")
}
func (p refusingPorts) ReleaseBinding(context.Context, string) error {
	p.t.Error("an adopted binding was released")
	return errors.New("release binding")
}
func (p refusingPorts) Release(context.Context, string) error {
	p.t.Error("an adopted enrollment was released by the provisioned path")
	return errors.New("release")
}
func (p refusingPorts) Retire(context.Context, string) error {
	p.t.Error("an adopted enrollment was retired")
	return errors.New("retire")
}
func (p refusingPorts) Deliver(context.Context, teams.Delivery) error { return nil }
func (p refusingPorts) Name(run string) (string, error)               { return teamstore.ChannelName(run) }

// fleet is the registry as the team host's Adopter sees it: live sessions,
// bound and owned, whose existence the team must never change.
type fleet struct {
	mu       sync.Mutex
	live     map[mesh.URN]teamhost.Adoption
	released []string
}

func newFleet() *fleet {
	f := &fleet{live: map[mesh.URN]teamhost.Adoption{}}
	for i, actor := range []mesh.URN{delegateURN, workerOne, workerTwo, strangerURN} {
		f.live[actor] = teamhost.Adoption{Actor: actor, AgentID: string(actor), Kind: mesh.ActorAgent, SessionID: "session-" + string(rune('a'+i)), Generation: 7, Resolved: true, Owner: operatorURN, LaunchID: "tether-claude", Pin: adoptPin}
	}
	return f
}
func (f *fleet) Verify(_ context.Context, c teamhost.AdoptionCheck) (teamhost.Adoption, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.live[c.Actor]
	if !ok {
		return teamhost.Adoption{}, teams.ErrNotFound
	}
	return v, nil
}
func (f *fleet) Release(_ context.Context, _ string, m teams.Member) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, m.SessionID)
	return nil
}
func (f *fleet) stillLive(actor mesh.URN, session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.live[actor]
	return ok && v.Resolved && v.SessionID == session && v.Generation == 7
}

// hostAdopter is the production shape: the service command goes to the real host.
type hostAdopter struct{ host *teamhost.Host }

func (a hostAdopter) Adopt(ctx context.Context, c teamsvc.AdoptCommand) (teams.Member, error) {
	return a.host.Adopt(ctx, teamhost.AdoptRequest{
		Provision:         teams.ProvisionRequest{IdempotencyKey: c.Key, RunID: c.RunID, MemberID: c.MemberID, Slot: c.Slot, Identity: c.Actor, Limits: c.Limits},
		SessionID:         c.SessionID,
		BindingGeneration: c.BindingGeneration,
		Principal:         c.Principal,
	})
}

type opPrincipals struct{}

func (opPrincipals) ResolvePrincipal(ctx context.Context) (teamsvc.Principal, error) {
	p, _ := ctx.Value(principalCtx{}).(teamsvc.Principal)
	return p, nil
}

type principalCtx struct{}

func as(p teamsvc.Principal) context.Context {
	return context.WithValue(context.Background(), principalCtx{}, p)
}

var operator = teamsvc.Principal{ID: operatorURN, Kind: mesh.ActorUser, Verified: true, LocalOperator: true}

func agent(id mesh.URN) teamsvc.Principal {
	return teamsvc.Principal{ID: id, Kind: mesh.ActorAgent, Verified: true}
}

type permissive struct{}

func (permissive) FormationCeilings(context.Context, teamsvc.Principal) (teamsvc.FormationPolicy, error) {
	return teamsvc.FormationPolicy{Limits: limitsFor(), MaxSlots: 4, MaxMembersPerSlot: 4, MaxInitialMembers: 4, AllowedPermissions: []teams.Permission{teams.MayMessage, teams.MayAssign, teams.MayDelegate}, AllowAdoptedSlots: true}, nil
}

func (permissive) AdmitAdoption(context.Context, teamsvc.Principal, teams.Team, teams.Slot) error {
	return nil
}

func limitsFor() mesh.Limits {
	return mesh.Limits{MaxDepth: 2, MaxChildren: 4, FanOut: 4, Budget: 10, Timeout: time.Minute, MaxRounds: 16, MaxStalls: 4}
}

func team(coordinatorGrants ...teams.Permission) teams.Team {
	grants := []teams.Grant{{FromSlot: "workers", Verb: teams.MayMessage, ToSlot: "coordinator"}}
	for _, v := range coordinatorGrants {
		grants = append(grants, teams.Grant{FromSlot: "coordinator", Verb: v, ToSlot: "workers"})
	}
	return teams.Team{ID: "fleet-team", Name: "Fleet", Version: 1,
		Slots: []teams.Slot{
			{Name: "coordinator", Definition: adoptPin, Resolution: teams.Pool, Pool: "lead", Identities: []mesh.URN{delegateURN, "msg://agent/fleet/lead-2"}, Activation: teams.Concurrent, Min: 0, Max: 2},
			{Name: "workers", Definition: adoptPin, Resolution: teams.Pool, Pool: "fleet", Identities: []mesh.URN{workerOne, workerTwo}, Activation: teams.Concurrent, Min: 0, Max: 2},
		},
		Phases:    []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"coordinator", "workers"}, OwnerSlot: "coordinator", ExitTrigger: teams.Trigger{Kind: "manual", Spec: map[string]string{"signal": "done"}}}},
		Authority: teams.Authority{Mode: teams.Strict, Grants: grants},
		Policy:    teams.Policy{Spawn: limitsFor(), History: mesh.HistoryNone},
		Routing:   teams.Routing{CoordinatorSlot: "coordinator"},
	}
}

type rig struct {
	svc     *teamsvc.Service
	storage *teamstore.Store
	host    *teamhost.Host
	fleet   *fleet
	run     teams.TeamRun
}

func newRig(t *testing.T, def teams.Team, ceilings teamsvc.Ceilings) *rig {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	storage, err := teamstore.New(db.DB(), teamstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fl := newFleet()
	ports := refusingPorts{t}
	opts := teamhost.Options{TrustTiers: map[mesh.DefinitionRef]teamhost.TrustTier{adoptPin: teamhost.TrustTrusted}, ActorTrust: map[mesh.URN]teamhost.TrustTier{operatorURN: teamhost.TrustTrusted}, AdoptableLaunches: teamhost.AdoptedLaunches{"tether-claude": adoptPin}}
	host, err := teamhost.New(db.DB(), storage, teamhost.Ports{Sessions: ports, Enroller: ports, Messenger: ports, Channels: ports, Adopter: fl}, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = storage.PutDefinition(ctx, def); err != nil {
		t.Fatal(err)
	}
	run, err := storage.CreateRun(ctx, teams.TeamRun{ID: "run-1", TeamID: def.ID, TeamVersion: def.Version, Channel: "team/run-1", Status: mesh.TaskWorking})
	if err != nil {
		t.Fatal(err)
	}
	err = storage.Mutate(ctx, "run-1", func(r *teams.Roster) error {
		r.Members = append(r.Members, teams.Member{ID: "owner", Slot: "coordinator", Actor: operatorURN, Kind: mesh.ActorUser, Status: "active", Governance: teams.Owner, JoinedAt: time.Now()})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := teamsvc.New(teamsvc.Deps{Principals: opPrincipals{}, Ceilings: ceilings, Runs: runStore{storage}, Calls: host, Definitions: storage, Roster: storage, Ledger: storage, Signals: storage, Provisioner: host, Adopter: hostAdopter{host}, Workflows: host, Sender: host, Routing: host, Trust: host, Approvals: host, Clock: host, IDs: host, Defaults: limitsFor()})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{svc, storage, host, fl, run.Run}
}

type runStore struct{ s *teamstore.Store }

func (r runStore) GetRun(ctx context.Context, id string) (teams.TeamRun, error) {
	c, err := r.s.GetRun(ctx, id)
	return c.Run, err
}
func (r runStore) MarkDissolved(ctx context.Context, id string) error {
	return r.s.MarkRunDissolved(ctx, id)
}

func (r *rig) adopt(t *testing.T, key string, actor mesh.URN, slot string) (teamsvc.Result, error) {
	t.Helper()
	session := r.fleet.live[actor].SessionID
	return r.svc.Adopt(as(operator), teamsvc.AdoptRequest{Key: key, RunID: "run-1", Slot: slot, Actor: actor, SessionID: session, BindingGeneration: 7})
}
func (r *rig) member(t *testing.T, actor mesh.URN) (teams.Member, bool) {
	t.Helper()
	roster, err := r.storage.Snapshot(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range roster.Members {
		if m.Actor == actor {
			return m, true
		}
	}
	return teams.Member{}, false
}

func TestOperatorAdoptsFleetIntoWorkersAndDelegateCoordinator(t *testing.T) {
	r := newRig(t, team(teams.MayMessage, teams.MayAssign), permissive{})
	for _, c := range []struct {
		key   string
		actor mesh.URN
		slot  string
	}{{"a1", delegateURN, "coordinator"}, {"a2", workerOne, "workers"}, {"a3", workerTwo, "workers"}} {
		res, err := r.adopt(t, c.key, c.actor, c.slot)
		if err != nil {
			t.Fatalf("adopt %s: %v", c.actor, err)
		}
		if res.Member == nil || res.Member.Actor != c.actor || res.Member.Slot != c.slot {
			t.Fatalf("result: %+v", res)
		}
		m, ok := r.member(t, c.actor)
		if !ok || m.Governance != teams.MemberRole || m.SpawnCapable || m.Status != "active" || m.SessionID != r.fleet.live[c.actor].SessionID {
			t.Fatalf("roster member: %+v", m)
		}
	}
	// The operator is still the run owner; the delegate is a plain member.
	if owner, _ := r.member(t, operatorURN); owner.Governance != teams.Owner {
		t.Fatal("operator lost ownership")
	}
	// Replaying a completed adoption returns the journaled answer, no duplicate.
	if _, err := r.adopt(t, "a2", workerOne, "workers"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	roster, _ := r.storage.Snapshot(context.Background(), "run-1")
	if len(roster.Members) != 4 {
		t.Fatalf("members after replay: %d", len(roster.Members))
	}
}

func TestDissolveReleasesAdoptedMembersAndKeepsSessionsAliveAndBound(t *testing.T) {
	r := newRig(t, team(teams.MayMessage, teams.MayAssign), permissive{})
	for _, c := range []struct {
		key   string
		actor mesh.URN
		slot  string
	}{{"a1", delegateURN, "coordinator"}, {"a2", workerOne, "workers"}} {
		if _, err := r.adopt(t, c.key, c.actor, c.slot); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.svc.Dissolve(as(operator), teamsvc.RunRequest{Key: "end", RunID: "run-1"}); err != nil {
		t.Fatalf("dissolve: %v", err)
	}
	for _, actor := range []mesh.URN{delegateURN, workerOne} {
		m, _ := r.member(t, actor)
		if m.Status != "released" {
			t.Fatalf("%s status %q, want released", actor, m.Status)
		}
		if !r.fleet.stillLive(actor, r.fleet.live[actor].SessionID) {
			t.Fatalf("%s lost its session or binding", actor)
		}
	}
	if len(r.fleet.released) != 2 {
		t.Fatalf("adopter releases: %v", r.fleet.released)
	}
	// refusingPorts fails the test if Stop, ReleaseBinding or Release were called.
}

func TestRemovingAdoptedDelegateKeepsRunAndOwnerAndSession(t *testing.T) {
	r := newRig(t, team(teams.MayMessage, teams.MayAssign), permissive{})
	res, err := r.adopt(t, "a1", delegateURN, "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.adopt(t, "a2", workerOne, "workers"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.svc.RemoveMember(as(operator), teamsvc.RemoveMemberRequest{Key: "revoke", RunID: "run-1", MemberID: res.Member.ID}); err != nil {
		t.Fatalf("revoke delegation: %v", err)
	}
	if m, _ := r.member(t, delegateURN); m.Status != "released" {
		t.Fatalf("delegate status %q", m.Status)
	}
	if w, _ := r.member(t, workerOne); w.Status != "active" {
		t.Fatal("removing the delegate affected a worker")
	}
	if owner, _ := r.member(t, operatorURN); owner.Status != "active" || owner.Governance != teams.Owner {
		t.Fatal("owner affected")
	}
	if c, err := r.storage.GetRun(context.Background(), "run-1"); err != nil || c.Run.Status == teamstore.RunDissolved {
		t.Fatalf("run ended by revoking the delegate: %v %v", c.Run.Status, err)
	}
	if !r.fleet.stillLive(delegateURN, r.fleet.live[delegateURN].SessionID) {
		t.Fatal("delegate session lost")
	}
}

func TestDelegateCannotDissolveRemoveOrAdopt(t *testing.T) {
	r := newRig(t, team(teams.MayMessage, teams.MayAssign), permissive{})
	res, err := r.adopt(t, "a1", delegateURN, "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.adopt(t, "a2", workerOne, "workers"); err != nil {
		t.Fatal(err)
	}
	delegate := as(agent(delegateURN))
	if _, err = r.svc.Dissolve(delegate, teamsvc.RunRequest{Key: "d", RunID: "run-1"}); err == nil {
		t.Fatal("delegate dissolved the run")
	}
	if _, err = r.svc.RemoveMember(delegate, teamsvc.RemoveMemberRequest{Key: "r", RunID: "run-1", MemberID: res.Member.ID}); err == nil {
		t.Fatal("delegate removed a member")
	}
	if _, err = r.svc.Adopt(delegate, teamsvc.AdoptRequest{Key: "x", RunID: "run-1", Slot: "workers", Actor: workerTwo, SessionID: r.fleet.live[workerTwo].SessionID, BindingGeneration: 7}); !errors.Is(err, teamsvc.ErrDenied) {
		t.Fatalf("delegate adopted: %v", err)
	}
	// Its message/assign/delegate grants are asserted via teams.Authorize in
	// TestNoAdoptedCoordinatorSeatCanHoldOrUseDelegate.
}

func TestCoordinatorSeatRefusesGrantsBeyondMessageAndAssign(t *testing.T) {
	for _, extra := range []teams.Permission{teams.MayDelegate, teams.MayAdmin, teams.MaySpawn} {
		r := newRig(t, team(teams.MayMessage, teams.MayAssign, extra), permissive{})
		if _, err := r.adopt(t, "a1", delegateURN, "coordinator"); !errors.Is(err, teamsvc.ErrDenied) {
			t.Fatalf("%s: %v", extra, err)
		}
		// The same team still adopts workers: the limit is the coordinator seat's.
		if _, err := r.adopt(t, "a2", workerOne, "workers"); err != nil {
			t.Fatalf("workers: %v", err)
		}
	}
}

func TestAdoptionRefusals(t *testing.T) {
	r := newRig(t, team(teams.MayMessage), permissive{})
	good := func() teamsvc.AdoptRequest {
		return teamsvc.AdoptRequest{Key: "k", RunID: "run-1", Slot: "workers", Actor: workerOne, SessionID: r.fleet.live[workerOne].SessionID, BindingGeneration: 7}
	}
	cases := []struct {
		name string
		ctx  context.Context
		mut  func(*teamsvc.AdoptRequest)
		want error
	}{
		{"no generation", as(operator), func(q *teamsvc.AdoptRequest) { q.BindingGeneration = 0 }, teamsvc.ErrInvalidRequest},
		{"no session", as(operator), func(q *teamsvc.AdoptRequest) { q.SessionID = "" }, teamsvc.ErrInvalidRequest},
		{"unknown slot", as(operator), func(q *teamsvc.AdoptRequest) { q.Slot = "reviewer" }, teamsvc.ErrInvalidRequest},
		{"actor the slot does not declare", as(operator), func(q *teamsvc.AdoptRequest) {
			q.Actor = strangerURN
			q.SessionID = r.fleet.live[strangerURN].SessionID
		}, teams.ErrProvisionFailed},
		{"stale generation", as(operator), func(q *teamsvc.AdoptRequest) { q.BindingGeneration = 6 }, teamsvc.ErrConflict},
		{"another session", as(operator), func(q *teamsvc.AdoptRequest) { q.SessionID = "not-theirs" }, teamsvc.ErrConflict},
		{"unauthenticated", context.Background(), func(*teamsvc.AdoptRequest) {}, teamsvc.ErrUnauthenticated},
		{"not a member of the run", as(agent(strangerURN)), func(*teamsvc.AdoptRequest) {}, teamsvc.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := good()
			q.Key = "k-" + tc.name
			tc.mut(&q)
			if _, err := r.svc.Adopt(tc.ctx, q); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if _, ok := r.member(t, workerOne); ok {
		t.Fatal("a refused adoption left a member behind")
	}
}

func TestSlotCardinalityAndOneSeatPerActor(t *testing.T) {
	d := team(teams.MayMessage)
	d.Slots[1].Max = 1
	d.Slots[1].Identities = []mesh.URN{workerOne, workerTwo}
	r := newRig(t, d, permissive{})
	if _, err := r.adopt(t, "a1", workerOne, "workers"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.adopt(t, "a2", workerTwo, "workers"); !errors.Is(err, teamsvc.ErrConflict) {
		t.Fatalf("full slot: %v", err)
	}
	if _, err := r.adopt(t, "a3", workerOne, "workers"); !errors.Is(err, teamsvc.ErrConflict) {
		t.Fatalf("same actor twice: %v", err)
	}
	if _, ok := r.member(t, workerTwo); ok {
		t.Fatal("refused adoption joined the roster")
	}
}

// bareCeilings has no adoption gate at all.
type bareCeilings struct{}

func (bareCeilings) FormationCeilings(context.Context, teamsvc.Principal) (teamsvc.FormationPolicy, error) {
	return permissive{}.FormationCeilings(context.Background(), teamsvc.Principal{})
}

func TestCeilingsWithoutAnAdoptionGateRefuseAdoption(t *testing.T) {
	r := newRig(t, team(teams.MayMessage), bareCeilings{})
	if _, err := r.adopt(t, "a1", workerOne, "workers"); !errors.Is(err, teamsvc.ErrDenied) {
		t.Fatalf("got %v, want denied", err)
	}
	if _, ok := r.member(t, workerOne); ok {
		t.Fatal("member joined without an adoption gate")
	}
}

type pausedCeilings struct{ permissive }

func (pausedCeilings) AdmitAdoption(context.Context, teamsvc.Principal, teams.Team, teams.Slot) error {
	return teamsvc.ErrUnavailable
}

func TestAdoptionPolicyGateRunsBeforeTheHost(t *testing.T) {
	r := newRig(t, team(teams.MayMessage), pausedCeilings{})
	if _, err := r.adopt(t, "a1", workerOne, "workers"); !errors.Is(err, teamsvc.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, ok := r.member(t, workerOne); ok {
		t.Fatal("member joined despite policy")
	}
}

func TestAdoptWithoutAdopterIsUnavailable(t *testing.T) {
	r := newRig(t, team(teams.MayMessage), permissive{})
	d := teamsvc.Deps{Principals: opPrincipals{}, Ceilings: permissive{}, Runs: runStore{r.storage}, Calls: r.host, Definitions: r.storage, Roster: r.storage, Ledger: r.storage, Signals: r.storage, Provisioner: r.host, Workflows: r.host, Sender: r.host, Routing: r.host, Trust: r.host, Approvals: r.host, Clock: r.host, IDs: r.host, Defaults: limitsFor()}
	svc, err := teamsvc.New(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Adopt(as(operator), teamsvc.AdoptRequest{Key: "k", RunID: "run-1", Slot: "workers", Actor: workerOne, SessionID: "s", BindingGeneration: 7}); !errors.Is(err, teamsvc.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

// Every identity the coordinator pool declares gets the slot's grants and no
// more: the spare is exactly as limited as the first delegate, and no
// coordinator seat can ever hold or exercise may_delegate.
func TestNoAdoptedCoordinatorSeatCanHoldOrUseDelegate(t *testing.T) {
	const spare mesh.URN = "msg://agent/fleet/lead-2"
	for _, actor := range []mesh.URN{delegateURN, spare} {
		t.Run(string(actor), func(t *testing.T) {
			// Refused outright when the slot's grants include may_delegate.
			bad := newRig(t, team(teams.MayMessage, teams.MayAssign, teams.MayDelegate), permissive{})
			bad.fleet.live[spare] = teamhost.Adoption{Actor: spare, AgentID: string(spare), Kind: mesh.ActorAgent, SessionID: "session-spare", Generation: 7, Resolved: true, Owner: operatorURN, LaunchID: "tether-claude", Pin: adoptPin}
			if _, err := bad.adopt(t, "bad", actor, "coordinator"); !errors.Is(err, teamsvc.ErrDenied) {
				t.Fatalf("adopted with may_delegate granted: %v", err)
			}
			// Adopted with message+assign, it can neither re-delegate nor assign
			// beyond its grants.
			ok := newRig(t, team(teams.MayMessage, teams.MayAssign), permissive{})
			ok.fleet.live[spare] = bad.fleet.live[spare]
			if _, err := ok.adopt(t, "ok", actor, "coordinator"); err != nil {
				t.Fatalf("adopt: %v", err)
			}
			if _, err := ok.adopt(t, "w", workerOne, "workers"); err != nil {
				t.Fatal(err)
			}
			from, _ := ok.member(t, actor)
			to, _ := ok.member(t, workerOne)
			def := team(teams.MayMessage, teams.MayAssign)
			for verb, want := range map[teams.Permission]error{teams.MayMessage: nil, teams.MayAssign: nil, teams.MayDelegate: teams.ErrDenied, teams.MayAdmin: teams.ErrDenied, teams.MaySpawn: teams.ErrDenied} {
				err := teams.Authorize(def, from, to, verb, "")
				if want == nil && err != nil || want != nil && !errors.Is(err, want) {
					t.Fatalf("%s for the adopted coordinator: got %v, want %v", verb, err, want)
				}
			}
		})
	}
}
