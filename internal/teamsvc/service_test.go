package teamsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/substrate/mesh/teams/memory"
)

type principalKey struct{}
type principals struct{}

func (principals) ResolvePrincipal(ctx context.Context) (Principal, error) {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p, nil
}
func caller(p Principal) context.Context {
	return context.WithValue(context.Background(), principalKey{}, p)
}

type ceilings struct {
	policy FormationPolicy
	err    error
}

func (c ceilings) FormationCeilings(context.Context, Principal) (FormationPolicy, error) {
	return c.policy, c.err
}

type runs struct{ run teams.TeamRun }

func (r runs) GetRun(_ context.Context, id string) (teams.TeamRun, error) {
	if id != r.run.ID {
		return teams.TeamRun{}, ErrNotFound
	}
	return r.run, nil
}

type calls struct {
	mu           sync.Mutex
	records      map[Scope]Record
	failComplete bool
	failPlan     bool
}

func newCalls() *calls { return &calls{records: map[Scope]Record{}} }
func copyRecord(r Record) Record {
	r.Intent.Request = bytes.Clone(r.Intent.Request)
	r.Plan = bytes.Clone(r.Plan)
	r.Result = bytes.Clone(r.Result)
	return r
}
func (c *calls) GetOrCreate(ctx context.Context, intent Intent) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.records[intent.Scope]; ok {
		if old.Intent.Digest != intent.Digest || !bytes.Equal(old.Intent.Request, intent.Request) {
			return Record{}, ErrConflict
		}
		return copyRecord(old), nil
	}
	r := copyRecord(Record{Intent: intent})
	c.records[intent.Scope] = r
	return copyRecord(r), nil
}
func (c *calls) SetPlan(ctx context.Context, scope Scope, payload json.RawMessage) error {
	return c.write(ctx, scope, payload, false)
}
func (c *calls) Complete(ctx context.Context, scope Scope, payload json.RawMessage) error {
	return c.write(ctx, scope, payload, true)
}
func (c *calls) write(ctx context.Context, scope Scope, payload json.RawMessage, result bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !result && c.failPlan {
		c.failPlan = false
		return errors.New("plan unavailable")
	}
	if result && c.failComplete {
		c.failComplete = false
		return errors.New("lost completion")
	}
	record, ok := c.records[scope]
	if !ok {
		return ErrNotFound
	}
	old := record.Plan
	if result {
		old = record.Result
	}
	if old != nil && !bytes.Equal(old, payload) {
		return ErrConflict
	}
	if result {
		record.Result = bytes.Clone(payload)
	} else {
		record.Plan = bytes.Clone(payload)
	}
	c.records[scope] = record
	return nil
}

func bounds() mesh.Limits {
	return mesh.Limits{MaxDepth: 3, MaxChildren: 8, FanOut: 8, Budget: 100, Timeout: time.Minute, MaxRounds: 16, MaxStalls: 4}
}

const ownerURN mesh.URN = "msg://agent/test/owner"
const workerURN mesh.URN = "msg://agent/test/worker"
const peerURN mesh.URN = "msg://agent/test/peer"

func policy() FormationPolicy {
	return FormationPolicy{Limits: bounds(), MaxSlots: 4, MaxMembersPerSlot: 4, MaxInitialMembers: 4, AllowedPermissions: []teams.Permission{teams.MayMessage, teams.MayAssign, teams.MayDelegate, teams.MayAdmin, teams.MaySpawn}}
}
func formationDefinition(p Principal) teams.Team {
	t := definition()
	if p.ID != ownerURN {
		t.Phases[0].OwnerSlot = "workers"
		t.Routing.CoordinatorSlot = "workers"
	}
	return t
}
func definition() teams.Team {
	return teams.Team{ID: "team", Name: "Team", Version: 1,
		Slots: []teams.Slot{
			{Name: "owner", Resolution: teams.Durable, Identity: ownerURN, Activation: teams.Singleton, Min: 1, Max: 1},
			{Name: "workers", Resolution: teams.Pool, Pool: "pool", Identities: []mesh.URN{workerURN, peerURN, "msg://agent/test/spare"}, Activation: teams.Concurrent, Min: 0, Max: 3},
		},
		Phases: []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"owner", "workers"}, OwnerSlot: "owner", ExitTrigger: teams.Trigger{Kind: "event", Spec: map[string]string{"event": "done"}}}},
		Authority: teams.Authority{Mode: teams.Strict, Grants: []teams.Grant{
			{FromSlot: "owner", Verb: teams.MayMessage, ToSlot: "workers"},
			{FromSlot: "owner", Verb: teams.MayAssign, ToSlot: "workers"},
			{FromSlot: "owner", Verb: teams.MayDelegate, ToSlot: "workers"},
			{FromSlot: "owner", Verb: teams.MaySpawn, ToSlot: "workers"},
		}}, Policy: teams.Policy{Spawn: bounds()}, Routing: teams.Routing{CoordinatorSlot: "owner"}}
}

type fixture struct {
	svc     *Service
	host    *memory.Host
	journal *calls
	team    teams.Team
	run     teams.TeamRun
}

func fixtureNew(t *testing.T) *fixture {
	t.Helper()
	h := memory.New()
	team := definition()
	ctx := context.Background()
	for _, id := range []mesh.URN{ownerURN, workerURN, peerURN, "msg://agent/test/spare"} {
		must(t, h.EnrollIdentity(ctx, id))
	}
	must(t, h.PutDefinition(ctx, team))
	roster := teams.Roster{RunID: "run", Members: []teams.Member{
		{ID: "owner", Slot: "owner", Actor: ownerURN, Kind: mesh.ActorAgent, SessionID: "owner-session", Status: "active", Governance: teams.Owner, Resolution: teams.Durable, Limits: bounds(), Budget: 100, SpawnCapable: true, Enrolled: true},
		{ID: "worker", Slot: "workers", Actor: workerURN, Kind: mesh.ActorAgent, SessionID: "worker-session", Status: "active", Governance: teams.MemberRole, Resolution: teams.Pool, Limits: bounds(), Budget: 100, SpawnCapable: true, Enrolled: true},
		{ID: "peer", Slot: "workers", Actor: peerURN, Kind: mesh.ActorAgent, SessionID: "peer-session", Status: "active", Governance: teams.MemberRole, Resolution: teams.Pool, Limits: bounds(), Budget: 100, SpawnCapable: true, Enrolled: true},
	}}
	must(t, h.Mutate(ctx, "run", func(r *teams.Roster) error { *r = roster; return nil }))
	run := teams.TeamRun{ID: "run", TeamID: team.ID, TeamVersion: 1, Channel: "team/run", Status: mesh.TaskWorking}
	c := newCalls()
	svc, err := New(Deps{Principals: principals{}, Ceilings: ceilings{policy: policy()}, Runs: runs{run}, Calls: c, Definitions: h, Roster: h, Ledger: h, Signals: h, Provisioner: h, Workflows: h, Triggers: h, Sender: h, Routing: h, Trust: h, Approvals: h, Clock: h, IDs: h, Defaults: bounds()})
	must(t, err)
	return &fixture{svc, h, c, team, run}
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func verified(id mesh.URN) Principal { return Principal{ID: id, Kind: mesh.ActorAgent, Verified: true} }

func TestAuthorityMatrix(t *testing.T) {
	verbs := []string{"form", "dissolve", "add", "remove", "assign", "delegate", "address", "cancel", "result"}
	for _, verb := range verbs {
		t.Run(verb, func(t *testing.T) {
			for _, mode := range []string{"allowed", "denied", "unverified", "operator", "stranger"} {
				t.Run(mode, func(t *testing.T) {
					f := fixtureNew(t)
					p := verified(ownerURN)
					var expected error
					if mode == "denied" {
						p = verified(peerURN)
						expected = ErrDenied
					}
					if mode == "unverified" {
						p.Verified = false
						expected = ErrUnauthenticated
					}
					if mode == "stranger" {
						p = verified("msg://agent/test/stranger")
						expected = ErrNotFound
					}
					if mode == "operator" {
						p = Principal{ID: LocalOperator, Kind: mesh.ActorUser, LocalOperator: true}
						must(t, f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error {
							m := r.Members[0]
							m.ID = "operator"
							m.Actor = LocalOperator
							m.Kind = mesh.ActorUser
							m.SessionID = ""
							r.Members = append(r.Members, m)
							return nil
						}))
					}
					key := "matrix"
					run := "run"
					var err error
					switch verb {
					case "form":
						// Formation accepts any trusted principal but is subject to host ceilings.
						if mode == "denied" {
							f.svc.deps.Ceilings = ceilings{err: ErrDenied}
						}
						if mode == "stranger" {
							expected = nil
						}
						formed := formationDefinition(p)
						formed.ID = "formation"
						_, err = f.svc.Form(caller(p), FormRequest{Key: key, Team: formed})
					case "dissolve":
						_, err = f.svc.Dissolve(caller(p), RunRequest{key, run})
					case "add":
						_, err = f.svc.AddMember(caller(p), AddMemberRequest{Key: key, RunID: run, Slot: "workers", Limits: mesh.Limits{Budget: 5}})
					case "remove":
						_, err = f.svc.RemoveMember(caller(p), RemoveMemberRequest{key, run, "worker"})
					case "assign":
						_, err = f.svc.Assign(caller(p), MessageRequest{Key: key, RunID: run, Address: "@workers", Body: "work"})
					case "delegate":
						_, err = f.svc.Delegate(caller(p), MessageRequest{Key: key, RunID: run, Address: "@workers", Body: "work"})
					case "address":
						_, err = f.svc.Address(caller(p), MessageRequest{Key: key, RunID: run, Address: "@workers", Body: "hello"})
					case "cancel":
						_, err = f.svc.Cancel(caller(p), CancelRequest{key, run, "worker", true})
					case "result":
						address := string(workerURN)
						if mode == "operator" {
							address = string(LocalOperator)
						}
						// Actor grants, including operator grants, must be authored explicitly.
						if mode == "operator" {
							f.team.Authority.Grants = append(f.team.Authority.Grants, teams.Grant{FromSlot: "owner", Verb: teams.MayDelegate, ToActor: LocalOperator})
							f.svc.deps.Definitions = staticDefinition{f.team}
						}
						accepted, e := f.svc.Delegate(caller(verified(ownerURN)), MessageRequest{Key: "prepare", RunID: run, Address: address, Body: "work"})
						must(t, e)
						if mode == "allowed" {
							p = verified(workerURN)
						}
						_, err = f.svc.ReportResult(caller(p), ReportResultRequest{key, run, accepted.DeliveryKeys[0], "done"})
					}
					if expected == nil {
						must(t, err)
					} else if !errors.Is(err, expected) {
						t.Fatalf("want %v, got %v", expected, err)
					}
					if err != nil {
						var typedErr *Error
						if !errors.As(err, &typedErr) {
							t.Fatal("missing typed error")
						}
					}
				})
			}
		})
	}
}

type staticDefinition struct{ team teams.Team }

func (d staticDefinition) GetDefinition(context.Context, string, uint64) (teams.Team, error) {
	return d.team, nil
}
func (d staticDefinition) PutDefinition(context.Context, teams.Team) error { return ErrConflict }

func TestOperatorHasNoGrantOrMembershipBypass(t *testing.T) {
	f := fixtureNew(t)
	p := Principal{ID: LocalOperator, Kind: mesh.ActorUser, LocalOperator: true}
	_, err := f.svc.Address(caller(p), MessageRequest{Key: "stranger", RunID: "run", Address: "@workers", Body: "hello"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	must(t, f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error {
		r.Members = append(r.Members, teams.Member{ID: "operator", Slot: "workers", Actor: LocalOperator, Kind: mesh.ActorUser, Status: "active", Governance: teams.MemberRole})
		return nil
	}))
	_, err = f.svc.Assign(caller(p), MessageRequest{Key: "no-grant", RunID: "run", Address: "@workers", Body: "work"})
	if !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestIdempotencyAndLostAcknowledgement(t *testing.T) {
	f := fixtureNew(t)
	ctx := caller(verified(ownerURN))
	req := MessageRequest{Key: "same", RunID: "run", Address: "@workers", Body: "work"}
	f.journal.failComplete = true
	if _, err := f.svc.Assign(ctx, req); err == nil {
		t.Fatal("completion loss not returned")
	}
	delivered := f.host.Deliveries()
	if len(delivered) != 1 {
		t.Fatal(delivered)
	}
	// Recreate the service, keeping host journal and retained roster versions.
	restart, err := New(f.svc.deps)
	must(t, err)
	got, err := restart.Assign(ctx, req)
	must(t, err)
	if got.Recipients[0].ID != delivered[0].Recipient.ID || len(f.host.Deliveries()) != 1 {
		t.Fatal("retry rerouted or duplicated")
	}
	got.Recipients[0].ID = "corrupt"
	replay, err := restart.Assign(ctx, req)
	must(t, err)
	if replay.Recipients[0].ID == "corrupt" {
		t.Fatal("result aliases journal")
	}
	req.Body = "different"
	_, err = restart.Assign(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	req.Body = "work"
	roster, e := f.host.Snapshot(context.Background(), "run")
	must(t, e)
	must(t, f.host.Mutate(context.Background(), "different-run", func(r *teams.Roster) error { r.Members = roster.Members; return nil }))
	req.RunID = "different-run"
	_, err = restart.Assign(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatal("key not bound across runs", err)
	}
	req.RunID = "run"
	p := verified(ownerURN)
	p.Verified = false
	_, err = restart.Assign(caller(p), req)
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("retry skipped auth", err)
	}
}

func TestConcurrentCallerKey(t *testing.T) {
	f := fixtureNew(t)
	ctx := caller(verified(ownerURN))
	req := MessageRequest{Key: "parallel", RunID: "run", Address: "@workers", Body: "work"}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_, err := f.svc.Assign(ctx, req)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(f.host.Deliveries()) != 1 {
		t.Fatal("duplicate acceptance")
	}
}

func TestSpawnLimitsAndCascade(t *testing.T) {
	f := fixtureNew(t)
	ctx := caller(verified(ownerURN))
	_, err := f.svc.AddMember(ctx, AddMemberRequest{Key: "wide", RunID: "run", Slot: "workers", Limits: mesh.Limits{MaxDepth: 4}})
	if !errors.Is(err, ErrDenied) {
		t.Fatal("widened depth", err)
	}
	first, err := f.svc.AddMember(ctx, AddMemberRequest{Key: "spawn", RunID: "run", Slot: "workers", Limits: mesh.Limits{Budget: 5}})
	must(t, err)
	replay, err := f.svc.AddMember(ctx, AddMemberRequest{Key: "spawn", RunID: "run", Slot: "workers", Limits: mesh.Limits{Budget: 5}})
	must(t, err)
	if !reflect.DeepEqual(first, replay) {
		t.Fatal("spawn retry differs")
	}
	_, err = f.svc.AddMember(ctx, AddMemberRequest{Key: "full", RunID: "run", Slot: "workers", Limits: mesh.Limits{Budget: 5}})
	if err == nil {
		t.Fatal("slot maximum ignored")
	}
	_, err = f.svc.Cancel(ctx, CancelRequest{Key: "cascade", RunID: "run", MemberID: "owner", Cascade: true})
	must(t, err)
	roster, err := f.host.Snapshot(ctx, "run")
	must(t, err)
	for _, m := range roster.Members {
		if (m.ID == "owner" || m.ID == first.Member.ID) && m.Status == "active" {
			t.Fatal("cascade skipped descendant", m)
		}
	}
	_, err = f.svc.Cancel(ctx, CancelRequest{Key: "cascade", RunID: "run", MemberID: "owner", Cascade: true})
	must(t, err)
}

func TestFormationCeilings(t *testing.T) {
	mutations := map[string]func(*fixture, *FormRequest){
		"depth":    func(_ *fixture, r *FormRequest) { r.Team.Policy.Spawn.MaxDepth++ },
		"children": func(_ *fixture, r *FormRequest) { r.Team.Policy.Spawn.MaxChildren++ },
		"fanout":   func(_ *fixture, r *FormRequest) { r.Team.Policy.Spawn.FanOut++ },
		"budget":   func(_ *fixture, r *FormRequest) { r.Team.Policy.Spawn.Budget++ },
		"timeout":  func(_ *fixture, r *FormRequest) { r.Team.Policy.Spawn.Timeout += time.Minute },
		"slot maximum": func(f *fixture, _ *FormRequest) {
			p := policy()
			p.MaxMembersPerSlot = 2
			f.svc.deps.Ceilings = ceilings{policy: p}
		},
		"initial count": func(f *fixture, r *FormRequest) {
			p := policy()
			p.MaxInitialMembers = 1
			f.svc.deps.Ceilings = ceilings{policy: p}
			r.Launch.Counts = map[string]int{"workers": 2}
		},
		"dev open":      func(_ *fixture, r *FormRequest) { r.Team.Authority.Mode = teams.DevOpen },
		"implicit open": func(_ *fixture, r *FormRequest) { r.Team.Authority.Mode = "" },
		"grant": func(f *fixture, _ *FormRequest) {
			p := policy()
			p.AllowedPermissions = []teams.Permission{teams.MayMessage}
			f.svc.deps.Ceilings = ceilings{policy: p}
		},
		"trust":          func(f *fixture, _ *FormRequest) { f.host.Trust = teams.TrustDeny },
		"trust approval": func(f *fixture, _ *FormRequest) { f.host.Trust = teams.TrustApproval },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f := fixtureNew(t)
			req := FormRequest{Key: "ceilings", Team: definition()}
			mutate(f, &req)
			for _, p := range []Principal{verified(ownerURN), {ID: LocalOperator, Kind: mesh.ActorUser, LocalOperator: true}} {
				_, err := f.svc.Form(caller(p), req)
				if !errors.Is(err, ErrDenied) {
					t.Fatal("ceiling bypass", err)
				}
			}
			if len(f.host.Provisioned()) != 0 {
				t.Fatal("refused formation had side effects")
			}
		})
	}
	f := fixtureNew(t)
	d := f.svc.deps
	d.Ceilings = nil
	_, err := New(d)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatal("nil ceilings", err)
	}
}

func TestReportResultRetainsSessionAndTerminalState(t *testing.T) {
	for _, mode := range []string{"session changed", "canceled", "changed result", "lost ack"} {
		t.Run(mode, func(t *testing.T) {
			f := fixtureNew(t)
			got, err := f.svc.Delegate(caller(verified(ownerURN)), MessageRequest{Key: "task", RunID: "run", Address: string(workerURN), Body: "work"})
			must(t, err)
			req := ReportResultRequest{Key: "result", RunID: "run", DelegateKey: got.DeliveryKeys[0], Body: "done"}
			ctx := caller(verified(workerURN))
			switch mode {
			case "session changed":
				must(t, f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error { r.Members[1].SessionID = "replacement"; return nil }))
			case "canceled":
				must(t, f.host.EndDelegation(context.Background(), req.DelegateKey, mesh.TaskCanceled))
			case "changed result":
				_, err = f.svc.ReportResult(ctx, req)
				must(t, err)
				req.Body = "changed"
			case "lost ack":
				f.journal.failComplete = true
			}
			_, err = f.svc.ReportResult(ctx, req)
			if mode == "lost ack" {
				if err == nil {
					t.Fatal("loss ignored")
				}
				_, err = f.svc.ReportResult(ctx, req)
				must(t, err)
			} else if !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
		})
	}
}

func TestValidationAndTypedErrors(t *testing.T) {
	f := fixtureNew(t)
	_, err := f.svc.Assign(caller(verified(ownerURN)), MessageRequest{RunID: "run", Body: "work"})
	var e *Error
	if !errors.As(err, &e) || e.Code != "invalid_request" {
		t.Fatal(err)
	}
	_, err = f.svc.Form(caller(verified(ownerURN)), FormRequest{Key: "bad", Team: teams.Team{ID: "empty"}})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatal(err)
	}
	if fmt.Sprint(e) == "" {
		t.Fatal("empty diagnostic")
	}
}

func TestTerminationLostAcknowledgement(t *testing.T) {
	for _, verb := range []string{"cancel", "remove", "dissolve"} {
		t.Run(verb, func(t *testing.T) {
			f := fixtureNew(t)
			ctx := caller(verified(ownerURN))
			lost := true
			f.host.After = func(op, _ string) error {
				if op == "released" && lost {
					lost = false
					return errors.New("lost release acknowledgement")
				}
				return nil
			}
			act := func() (Result, error) {
				switch verb {
				case "cancel":
					return f.svc.Cancel(ctx, CancelRequest{Key: "end", RunID: "run", MemberID: "owner", Cascade: true})
				case "remove":
					return f.svc.RemoveMember(ctx, RemoveMemberRequest{Key: "end", RunID: "run", MemberID: "worker"})
				default:
					return f.svc.Dissolve(ctx, RunRequest{Key: "end", RunID: "run"})
				}
			}
			_, err := act()
			if err == nil {
				t.Fatal("ack loss ignored")
			}
			restarted, err := New(f.svc.deps)
			must(t, err)
			f.svc = restarted
			_, err = act()
			must(t, err)
			roster, err := f.host.Snapshot(context.Background(), "run")
			must(t, err)
			for _, m := range roster.Members {
				if verb == "dissolve" || m.ID == "owner" && verb == "cancel" || m.ID == "worker" && verb == "remove" {
					if m.Status == "active" || m.Status == "releasing" {
						t.Fatal("not ended", m)
					}
				}
			}
		})
	}
}

func TestDissolveChecksWholeRosterAndRejectsNewcomers(t *testing.T) {
	f := fixtureNew(t)
	// An ordinary member with only a subset grant cannot end the run.
	f.team.Authority.Grants = append(f.team.Authority.Grants, teams.Grant{FromActor: peerURN, Verb: teams.MayAdmin, ToActor: workerURN})
	f.svc.deps.Definitions = staticDefinition{f.team}
	_, err := f.svc.Dissolve(caller(verified(peerURN)), RunRequest{Key: "subset", RunID: "run"})
	if !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	roster, err := f.host.Snapshot(context.Background(), "run")
	must(t, err)
	for _, m := range roster.Members {
		if m.Status != "active" {
			t.Fatal("partial refusal", m)
		}
	}
	// Lost cleanup acknowledgement retains a bounded plan; no new member may
	// be swept into a retry of that previously authorized dissolution.
	f.host.Before = func(op, _ string) error {
		if op == "released" {
			return errors.New("cleanup unavailable")
		}
		return nil
	}
	_, err = f.svc.Dissolve(caller(verified(ownerURN)), RunRequest{Key: "all", RunID: "run"})
	if err == nil {
		t.Fatal("cleanup error ignored")
	}
	f.host.Before = nil
	must(t, f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error {
		r.Members = append(r.Members, teams.Member{ID: "new", Slot: "workers", Actor: "msg://agent/test/new", Kind: mesh.ActorAgent, Status: "active", SessionID: "new-session"})
		return nil
	}))
	_, err = f.svc.Dissolve(caller(verified(ownerURN)), RunRequest{Key: "all", RunID: "run"})
	if !errors.Is(err, ErrConflict) {
		t.Fatal("newcomer swept up", err)
	}
}

func TestExistingOpenRunRefused(t *testing.T) {
	f := fixtureNew(t)
	f.team.Authority.Mode = teams.DevOpen
	f.svc.deps.Definitions = staticDefinition{f.team}
	_, err := f.svc.Address(caller(verified(ownerURN)), MessageRequest{Key: "open", RunID: "run", Address: "@workers", Body: "hello"})
	if !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestTerminationJournalFailurePrecedesReservation(t *testing.T) {
	f := fixtureNew(t)
	ctx := caller(verified(ownerURN))
	req := CancelRequest{Key: "self", RunID: "run", MemberID: "owner", Cascade: true}
	f.journal.failPlan = true
	_, err := f.svc.Cancel(ctx, req)
	if err == nil {
		t.Fatal("journal failure ignored")
	}
	roster, err := f.host.Snapshot(context.Background(), "run")
	must(t, err)
	if roster.Members[0].Status != "active" {
		t.Fatal("caller stopped before journal commit")
	}
	_, err = f.svc.Cancel(ctx, req)
	must(t, err)
}

func TestCancellationPlanRace(t *testing.T) {
	f := fixtureNew(t)
	injected := false
	f.host.Before = func(op, _ string) error {
		if op == "roster" && !injected {
			injected = true
			return f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error {
				r.Members = append(r.Members, teams.Member{ID: "child", Slot: "workers", Actor: "msg://agent/test/child", Kind: mesh.ActorAgent, Status: "active", SessionID: "child-session", Parent: "owner"})
				return nil
			})
		}
		return nil
	}
	_, err := f.svc.Cancel(caller(verified(ownerURN)), CancelRequest{Key: "bound", RunID: "run", MemberID: "owner", Cascade: true})
	if !errors.Is(err, ErrConflict) {
		t.Fatal("plan widened after journal", err)
	}
	roster, err := f.host.Snapshot(context.Background(), "run")
	must(t, err)
	for _, m := range roster.Members {
		if m.Status != "active" {
			t.Fatal("partial cancellation", m)
		}
	}
}

func TestOperatorRequiresHostProofAndUnconfiguredIsInert(t *testing.T) {
	f := fixtureNew(t)
	asserted := Principal{ID: LocalOperator, Kind: mesh.ActorUser}
	_, err := f.svc.Form(caller(asserted), FormRequest{Key: "asserted", Team: definition()})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("operator urn accepted without host proof", err)
	}
	badProof := Principal{ID: ownerURN, Kind: mesh.ActorAgent, LocalOperator: true}
	_, err = f.svc.Form(caller(badProof), FormRequest{Key: "other", Team: definition()})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("operator bit accepted for another identity", err)
	}
	d := f.svc.deps
	d.Principals = nil
	inert, err := New(d)
	must(t, err)
	_, err = inert.Form(context.Background(), FormRequest{Key: "inert", Team: definition()})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing resolver not inert", err)
	}
}

func TestExternalOwnerCountsAgainstHostCeiling(t *testing.T) {
	f := fixtureNew(t)
	p := policy()
	p.MaxInitialMembers = 1
	f.svc.deps.Ceilings = ceilings{policy: p}
	_, err := f.svc.Form(caller(Principal{ID: LocalOperator, Kind: mesh.ActorUser, LocalOperator: true}), FormRequest{Key: "owner-count", Team: formationDefinition(Principal{ID: LocalOperator})})
	if !errors.Is(err, ErrDenied) {
		t.Fatal("external owner bypassed host membership bound", err)
	}
	if len(f.host.Provisioned()) != 0 {
		t.Fatal("rejected owner limit provisioned workers")
	}
}
