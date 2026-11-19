package teamruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/agentdef"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamspolicy"
	"github.com/hollis-labs/tether/internal/teamstore"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// Stage B pins: orchestrator and reviewer on Astra (claude), workers on Sol
// (claude, a different model on the same runtime) and an Agy-style third launch.
var (
	astraPin = mesh.DefinitionRef{ID: "claude-astra", Revision: "r1", Digest: "d-astra"}
	solPin   = mesh.DefinitionRef{ID: "claude-sol", Revision: "r1", Digest: "d-sol"}
	agyPin   = mesh.DefinitionRef{ID: "agy-agent", Revision: "r1", Digest: "d-agy"}
)

const dispatcher = "msg://service/torque/dispatcher"

const stageBPolicy = `version: 1
services: [msg://service/torque/dispatcher]
launches:
  - {launch: launch-astra, runtime: claude, model: astra, definition: {id: claude-astra, revision: r1, digest: d-astra}}
  - {launch: launch-sol, runtime: claude, model: sol, definition: {id: claude-sol, revision: r1, digest: d-sol}}
  - {launch: launch-agy, runtime: antigravity, definition: {id: agy-agent, revision: r1, digest: d-agy}}
`

type stageBDefinitions struct{}

func (stageBDefinitions) Load(_ context.Context, p mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
	if p != astraPin && p != solPin && p != agyPin {
		return definitionresolve.VerifiedDefinition{}, definitionresolve.ErrPinMismatch
	}
	return definitionresolve.VerifiedDefinition{Pin: p, Definition: &agentdef.Definition{Name: p.ID}}, nil
}

// actingAs lets a test speak as a team member (an agent session) while the
// operator and services still go through the real Principals adapter.
type actingAs struct {
	real Principals
}
type actorKey struct{}

func (a actingAs) ResolvePrincipal(c context.Context) (teamsvc.Principal, error) {
	if p, ok := c.Value(actorKey{}).(teamsvc.Principal); ok {
		return p, nil
	}
	return a.real.ResolvePrincipal(c)
}
func asMember(m teams.Member) context.Context {
	return context.WithValue(context.Background(), actorKey{}, teamsvc.Principal{ID: m.Actor, Kind: m.Kind, Verified: true})
}

type stageBFixture struct {
	*policyFixture
	db              *store.Store
	storage         *teamstore.Store
	state           func() map[string]int
	registerWorkers func(...string) []mesh.URN
}

func (f *stageBFixture) dbExec(q string, args ...any) (sql.Result, error) {
	return f.db.DB().Exec(q, args...)
}
func (f *stageBFixture) endReason(t *testing.T, run string) string {
	t.Helper()
	var reason string
	check(t, f.db.DB().QueryRow(`SELECT end_reason FROM team_run_deadlines WHERE run_id=?`, run).Scan(&reason))
	return reason
}
func (f *stageBFixture) sessionsByLaunch() (map[string]int, error) {
	out := map[string]int{}
	rows, err := f.db.DB().Query(`SELECT launch_id, count(*) FROM sessions GROUP BY launch_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var n int
		if err = rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// markMembersStopped records every provisioned member as stopped, as the
// reconciler does when a member's session ends on its own.
func (f *stageBFixture) markMembersStopped(run string) error {
	return f.storage.Mutate(ctx, run, func(r *teams.Roster) error {
		for i, m := range r.Members {
			if m.Governance != teams.Owner {
				r.Members[i].Status = "stopped"
			}
		}
		return nil
	})
}

func newStageBFixture(t *testing.T, raw string) (*stageBFixture, *teamhost.Host, func(string) int) {
	t.Helper()
	root := t.TempDir()
	check(t, writeFile(teamspolicy.Path(root), raw))
	policy, err := teamspolicy.Load(root)
	check(t, err)
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	targets := map[mesh.DefinitionRef]LaunchTarget{}
	for p, launch := range policy.LaunchTargets() {
		targets[p] = LaunchTarget{LaunchID: launch}
	}
	e, err := NewLegacyEnroller(db.DB(), s, reg, stageBDefinitions{}, targets)
	check(t, err)
	effects := &sessionEffects{db: db}
	sessions, err := NewSessions(db, s, effects, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	host, err := teamhost.New(db.DB(), s, teamhost.Ports{Enroller: e, Sessions: sessions, Messenger: messages, Channels: Channels{}}, policy.HostOptions())
	check(t, err)
	// Observe mode: identity enforcement is off; a policy-listed service is
	// still admitted, which is the point of the dispatcher path.
	principals := actingAs{Principals{Mode: identity.Observe, Sessions: e, Services: policy.IsService}}
	service, err := teamsvc.New(teamsvc.Deps{Ceilings: teamspolicy.Ceilings{Policy: policy}, Principals: principals, Runs: host, Calls: host, Definitions: s, Roster: s, Ledger: s, Signals: s, Provisioner: host, Workflows: host, Triggers: host, Sender: host, Routing: host, Trust: host, Approvals: host, Clock: host, IDs: host, Defaults: policy.MaxLimits(), Deadlines: host})
	check(t, err)
	count := func(table string) int {
		var n int
		check(t, db.DB().QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		return n
	}
	f := &stageBFixture{policyFixture: &policyFixture{reg: reg, svc: service, policy: policy, effects: effects, sessions: sessions}, db: db, storage: s}
	f.state = func() map[string]int {
		out := map[string]int{}
		rows, err := db.DB().Query(`SELECT state, count(*) FROM sessions GROUP BY state`)
		check(t, err)
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var st string
			var n int
			check(t, rows.Scan(&st, &n))
			out[st] = n
		}
		return out
	}
	f.registerWorkers = func(names ...string) []mesh.URN {
		raw, err := json.Marshal(solPin)
		check(t, err)
		var ids []mesh.URN
		for _, name := range names {
			profile, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: name, Props: map[string]string{"team_definition": string(raw)}}, "fixture", name)
			check(t, err)
			ids = append(ids, mesh.URN(profile.URN))
		}
		return ids
	}
	return f, host, count
}

func serviceContext(t *testing.T, id string) context.Context {
	t.Helper()
	return identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: id, Kind: "service", Scopes: []string{"team.write"}})
}

func stageBTeam(ids []mesh.URN) teams.Team {
	slot := func(name string, pin mesh.DefinitionRef, lo, hi int) teams.Slot {
		return teams.Slot{Name: name, Definition: pin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: lo, Max: hi}
	}
	workers := teams.Slot{Name: "workers", Definition: solPin, Resolution: teams.Pool, Pool: "workers", Identities: ids, Activation: teams.Concurrent, Min: 1, Max: len(ids)}
	var grants []teams.Grant
	for _, v := range []teams.Permission{teams.MayMessage, teams.MayAssign, teams.MayDelegate} {
		grants = append(grants,
			teams.Grant{FromSlot: "coordinator", Verb: v, ToSlot: "orchestrator"},
			teams.Grant{FromSlot: "orchestrator", Verb: v, ToSlot: "workers"},
			teams.Grant{FromSlot: "orchestrator", Verb: v, ToSlot: "reviewer"})
	}
	grants = append(grants,
		teams.Grant{FromSlot: "workers", Verb: teams.MayMessage, ToSlot: "orchestrator"},
		teams.Grant{FromSlot: "reviewer", Verb: teams.MayMessage, ToSlot: "orchestrator"})
	return teams.Team{ID: "stage-b", Name: "Stage B", Version: 1,
		Slots:     []teams.Slot{slot("coordinator", astraPin, 0, 1), slot("orchestrator", astraPin, 1, 1), workers, slot("reviewer", astraPin, 1, 1)},
		Phases:    []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"coordinator", "orchestrator", "workers", "reviewer"}, OwnerSlot: "coordinator", ExitTrigger: teams.Trigger{Kind: "manual", Spec: map[string]string{"signal": "done"}}}},
		Authority: teams.Authority{Mode: teams.Strict, Grants: grants},
		Policy:    teams.Policy{History: mesh.HistoryNone}, Routing: teams.Routing{CoordinatorSlot: "coordinator"}}
}

func memberBySlot(t *testing.T, f *stageBFixture, _ *teamhost.Host, run string, slot string, nth int) teams.Member {
	t.Helper()
	roster, err := f.storage.Snapshot(context.Background(), run)
	check(t, err)
	seen := 0
	for _, m := range roster.Members {
		if m.Slot == slot && m.Governance != teams.Owner {
			if seen == nth {
				return m
			}
			seen++
		}
	}
	t.Fatalf("no member %d in slot %s", nth, slot)
	return teams.Member{}
}

// A service (Torque's dispatcher) forms a four-agent team with per-slot
// launches, hands the orchestrator the task, the orchestrator reports the final
// result, and the run dissolves with no session left running.
func TestStageBServiceFormsTeamAndResultDissolvesIt(t *testing.T) {
	f, host, _ := newStageBFixture(t, stageBPolicy)
	ids := f.registerWorkers("w1", "w2")
	svc := serviceContext(t, dispatcher)
	team := stageBTeam(ids)
	formed, err := f.svc.Form(svc, teamsvc.FormRequest{Key: "dispatch-1", Team: team, Launch: teams.LaunchRequest{Counts: map[string]int{"workers": 2}}})
	check(t, err)
	if formed.Run == nil || f.effects.launched != 4 {
		t.Fatalf("want orchestrator, two workers and reviewer launched, got %d (%+v)", f.effects.launched, formed.Run)
	}
	rows, err := host.OpenRuns(ctx, 10)
	check(t, err)
	if len(rows) != 1 || rows[0].RunID != formed.Run.ID {
		t.Fatalf("deadline not recorded: %+v", rows)
	}
	// Defaults, not the old five-minute ceiling: the deadline is hours away.
	if until := time.Until(rows[0].Deadline); until < 3*time.Hour || until > 5*time.Hour {
		t.Fatalf("default timeout not applied: %s", until)
	}
	perLaunch, err := f.sessionsByLaunch()
	check(t, err)
	if perLaunch["launch-astra"] != 2 || perLaunch["launch-sol"] != 2 {
		t.Fatalf("slots did not resolve to their own launches: %v", perLaunch)
	}
	if got, ok := f.policy.PairingFor("dispatch-1"); !ok || !got.Different || got.WorkerModel != "sol" || got.ReviewerModel != "astra" {
		t.Fatalf("pairing: %+v", got)
	}

	// The service hands the task to the orchestrator; the orchestrator reports it.
	task, err := f.svc.Delegate(svc, teamsvc.MessageRequest{Key: "task", RunID: formed.Run.ID, Address: "@orchestrator", Body: "do the thing"})
	check(t, err)
	orch := memberBySlot(t, f, host, formed.Run.ID, "orchestrator", 0)
	// The orchestrator's report is the final result.
	_, err = f.svc.ReportResult(asMember(orch), teamsvc.ReportResultRequest{Key: "final", RunID: formed.Run.ID, DelegateKey: task.DeliveryKeys[0], Body: "done"})
	check(t, err)
	if reason := f.endReason(t, formed.Run.ID); reason != teamsvc.EndResult {
		t.Fatalf("final result did not request the end: %q", reason)
	}
	check(t, f.svc.ReapRuns(ctx, 10))
	for state, n := range f.state() {
		if state != "killed" && n > 0 {
			t.Fatalf("a session remains after dissolve: %v", f.state())
		}
	}
	if f.state()["killed"] != 4 {
		t.Fatalf("want 4 stopped sessions, got %v", f.state())
	}
	run, err := host.GetRun(ctx, formed.Run.ID)
	check(t, err)
	if run.Status != "dissolved" {
		t.Fatalf("run status %q", run.Status)
	}
	rows, err = host.OpenRuns(ctx, 10)
	check(t, err)
	if len(rows) != 0 {
		t.Fatal("dissolved run still open")
	}
	check(t, f.svc.ReapRuns(ctx, 10)) // idempotent
}

// A worker's report goes to the orchestrator and leaves the team running.
func TestStageBWorkerResultDoesNotEndRun(t *testing.T) {
	f, host, _ := newStageBFixture(t, stageBPolicy)
	svc := serviceContext(t, dispatcher)
	formed, err := f.svc.Form(svc, teamsvc.FormRequest{Key: "w", Team: stageBTeam(f.registerWorkers("w1", "w2")), Launch: teams.LaunchRequest{Counts: map[string]int{"workers": 2}}})
	check(t, err)
	orch := memberBySlot(t, f, host, formed.Run.ID, "orchestrator", 0)
	w1 := memberBySlot(t, f, host, formed.Run.ID, "workers", 0)
	job, err := f.svc.Delegate(asMember(orch), teamsvc.MessageRequest{Key: "job", RunID: formed.Run.ID, Address: string(w1.Actor), Body: "part"})
	check(t, err)
	_, err = f.svc.ReportResult(asMember(w1), teamsvc.ReportResultRequest{Key: "part-done", RunID: formed.Run.ID, DelegateKey: job.DeliveryKeys[0], Body: "ok"})
	check(t, err)
	if reason := f.endReason(t, formed.Run.ID); reason != "" {
		t.Fatalf("worker result requested the end: %q", reason)
	}
	check(t, f.svc.ReapRuns(ctx, 10))
	if f.state()["killed"] != 0 {
		t.Fatal("run was reaped after a worker result")
	}
}

// Workers and the reviewer reach only the orchestrator; nobody else.
func TestStageBWorkersAndReviewerReachOnlyTheOrchestrator(t *testing.T) {
	f, host, _ := newStageBFixture(t, stageBPolicy)
	svc := serviceContext(t, dispatcher)
	formed, err := f.svc.Form(svc, teamsvc.FormRequest{Key: "authority", Team: stageBTeam(f.registerWorkers("w1", "w2", "w3")), Launch: teams.LaunchRequest{Counts: map[string]int{"workers": 3}}})
	check(t, err)
	run := formed.Run.ID
	orch := memberBySlot(t, f, host, run, "orchestrator", 0)
	w1, w2 := memberBySlot(t, f, host, run, "workers", 0), memberBySlot(t, f, host, run, "workers", 1)
	rev := memberBySlot(t, f, host, run, "reviewer", 0)

	verbs := map[string]func(context.Context, teamsvc.MessageRequest) (teamsvc.Result, error){"address": f.svc.Address, "assign": f.svc.Assign, "delegate": f.svc.Delegate}
	for _, from := range []struct {
		name string
		m    teams.Member
	}{{"worker", w1}, {"reviewer", rev}} {
		targets := map[string]string{"orchestrator": "@orchestrator"}
		denied := map[string]string{"owner seat": "@coordinator", "everyone": "@all", "worker slot": "@workers", "reviewer slot": "@reviewer",
			"peer worker by urn": string(w2.Actor), "reviewer by urn": string(rev.Actor), "service by urn": dispatcher}
		if from.name == "reviewer" {
			denied["worker by urn"] = string(w1.Actor)
			delete(denied, "reviewer by urn")
		}
		if from.name == "worker" {
			denied["self"] = string(w1.Actor)
		}
		for verb, send := range verbs {
			for label, addr := range denied {
				if verb != "address" && (addr == "@all" || addr == "@workers") {
					continue // work verbs refuse multiple recipients outright
				}
				_, err := send(asMember(from.m), teamsvc.MessageRequest{Key: from.name + verb + label, RunID: run, Address: addr, Body: "hi"})
				if !errors.Is(err, teamsvc.ErrDenied) {
					t.Fatalf("%s %s -> %s (%s) was not denied: %v", from.name, verb, label, addr, err)
				}
			}
		}
		// The only open path is a plain message to the orchestrator.
		for label, addr := range targets {
			if _, err := f.svc.Address(asMember(from.m), teamsvc.MessageRequest{Key: from.name + "ok" + label, RunID: run, Address: addr, Body: "hi"}); err != nil {
				t.Fatalf("%s -> %s refused: %v", from.name, label, err)
			}
			for _, verb := range []string{"assign", "delegate"} {
				if _, err := verbs[verb](asMember(from.m), teamsvc.MessageRequest{Key: from.name + verb + "orch", RunID: run, Address: addr, Body: "hi"}); !errors.Is(err, teamsvc.ErrDenied) {
					t.Fatalf("%s may not %s the orchestrator: %v", from.name, verb, err)
				}
			}
		}
	}
	// The orchestrator directs workers and the reviewer.
	for _, addr := range []string{string(w1.Actor), string(w2.Actor), string(rev.Actor), "@workers"} {
		for verb, send := range verbs {
			if verb != "address" && addr == "@workers" {
				continue // work verbs need exactly one recipient
			}
			if _, err := send(asMember(orch), teamsvc.MessageRequest{Key: "orch" + verb + addr, RunID: run, Address: addr, Body: "go"}); err != nil {
				t.Fatalf("orchestrator %s -> %s: %v", verb, addr, err)
			}
		}
	}
}

// Timeout ceiling and all-members-stopped both end the run and leave no session.
func TestStageBHardStops(t *testing.T) {
	for _, mode := range []string{"timeout", "all stopped"} {
		t.Run(mode, func(t *testing.T) {
			f, host, _ := newStageBFixture(t, stageBPolicy)
			formed, err := f.svc.Form(serviceContext(t, dispatcher), teamsvc.FormRequest{Key: "stop", Team: stageBTeam(f.registerWorkers("w1", "w2")), Launch: teams.LaunchRequest{Counts: map[string]int{"workers": 2}}})
			check(t, err)
			check(t, f.svc.ReapRuns(ctx, 10))
			if f.state()["killed"] != 0 {
				t.Fatal("a healthy run was reaped early")
			}
			switch mode {
			case "timeout":
				_, err = f.dbExec(`UPDATE team_run_deadlines SET deadline=? WHERE run_id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), formed.Run.ID)
				check(t, err)
			case "all stopped":
				// Every member session ends on its own (crash or exit).
				_, err = f.dbExec(`UPDATE sessions SET state='killed'`)
				check(t, err)
				check(t, f.markMembersStopped(formed.Run.ID))
			}
			check(t, f.svc.ReapRuns(ctx, 10))
			run, err := host.GetRun(ctx, formed.Run.ID)
			check(t, err)
			if run.Status != "dissolved" {
				t.Fatalf("%s did not dissolve the run: %q", mode, run.Status)
			}
			for state, n := range f.state() {
				if state != "killed" && n > 0 {
					t.Fatalf("a session remains: %v", f.state())
				}
			}
		})
	}
}

// An unlisted service, an unverified listed one, and the paused gate are refused
// with no side effects, while the operator keeps working.
func TestStageBServiceAdmission(t *testing.T) {
	f, _, count := newStageBFixture(t, stageBPolicy)
	ids := f.registerWorkers("w1", "w2")
	form := func(c context.Context, key string) error {
		_, err := f.svc.Form(c, teamsvc.FormRequest{Key: key, Team: stageBTeam(ids), Launch: teams.LaunchRequest{Counts: map[string]int{"workers": 2}}})
		return err
	}
	if err := form(serviceContext(t, "msg://service/torque/other"), "unlisted"); err == nil {
		t.Fatal("unlisted service formed a team")
	}
	scopeless := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: dispatcher, Kind: "service", Scopes: []string{"session.write"}})
	if err := form(scopeless, "scopeless"); err == nil {
		t.Fatal("listed service without team.write formed a team")
	}
	unverified := identity.WithPrincipal(context.Background(), identity.Principal{Verified: false, ID: dispatcher, Kind: "service"})
	if err := form(unverified, "unverified"); err == nil {
		t.Fatal("unverified service formed a team")
	}
	if err := form(context.Background(), "anonymous"); err == nil {
		t.Fatal("anonymous caller formed a team")
	}
	if count("team_runs") != 0 || count("sessions") != 0 || f.effects.launched != 0 {
		t.Fatal("a refused formation had side effects")
	}
	f.policy.Pause()
	if err := form(serviceContext(t, dispatcher), "paused"); !errors.Is(err, teamsvc.ErrUnavailable) {
		t.Fatalf("pause did not refuse the service: %v", err)
	}
	f.policy.Resume()
	if err := form(serviceContext(t, dispatcher), "listed"); err != nil {
		t.Fatalf("listed service refused: %v", err)
	}
}
