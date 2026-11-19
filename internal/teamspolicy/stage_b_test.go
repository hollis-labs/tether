package teamspolicy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

var (
	astra = mesh.DefinitionRef{ID: "claude-astra", Revision: "r1", Digest: "d-astra"}
	sol   = mesh.DefinitionRef{ID: "claude-sol", Revision: "r1", Digest: "d-sol"}
	codex = mesh.DefinitionRef{ID: "codex-agent", Revision: "r1", Digest: "d-codex"}
)

const bPolicy = `version: 1
services: [msg://service/torque/dispatcher]
launches:
  - {launch: la, runtime: claude, model: astra, definition: {id: claude-astra, revision: r1, digest: d-astra}}
  - {launch: ls, runtime: claude, model: sol, definition: {id: claude-sol, revision: r1, digest: d-sol}}
  - {launch: lc, runtime: codex, definition: {id: codex-agent, revision: r1, digest: d-codex}}
`

const dispatcherURN = "msg://service/torque/dispatcher"

func svcPrincipal() teamsvc.Principal {
	return teamsvc.Principal{ID: dispatcherURN, Kind: mesh.ActorService, Verified: true}
}

func bTeam(workers int) (teams.Team, teams.LaunchRequest) {
	fresh := func(name string, pin mesh.DefinitionRef, lo, hi int) teams.Slot {
		return teams.Slot{Name: name, Definition: pin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: lo, Max: hi}
	}
	var ids []mesh.URN
	for i := 0; i < workers; i++ {
		ids = append(ids, mesh.URN("msg://agent/local/w"+string(rune('a'+i))))
	}
	w := teams.Slot{Name: "workers", Definition: sol, Resolution: teams.Pool, Pool: "workers", Identities: ids, Activation: teams.Concurrent, Min: 1, Max: len(ids)}
	t := teams.Team{ID: "b", Name: "B", Version: 1,
		Slots:  []teams.Slot{fresh("coordinator", astra, 0, 1), fresh("orchestrator", astra, 1, 1), w, fresh("reviewer", astra, 1, 1)},
		Phases: []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"coordinator", "orchestrator", "workers", "reviewer"}, OwnerSlot: "coordinator", ExitTrigger: teams.Trigger{Kind: "manual", Spec: map[string]string{"signal": "done"}}}},
		Authority: teams.Authority{Mode: teams.Strict, Grants: []teams.Grant{
			{FromSlot: "coordinator", Verb: teams.MayDelegate, ToSlot: "orchestrator"},
			{FromSlot: "orchestrator", Verb: teams.MayMessage, ToSlot: "workers"}, {FromSlot: "orchestrator", Verb: teams.MayAssign, ToSlot: "workers"},
			{FromSlot: "orchestrator", Verb: teams.MayDelegate, ToSlot: "reviewer"},
			{FromSlot: "workers", Verb: teams.MayMessage, ToSlot: "orchestrator"}, {FromSlot: "reviewer", Verb: teams.MayMessage, ToSlot: "orchestrator"}}},
		Routing: teams.Routing{CoordinatorSlot: "coordinator"}}
	return t, teams.LaunchRequest{Counts: map[string]int{"workers": workers}}
}

func ceilingsFor(t *testing.T, raw string) Ceilings {
	t.Helper()
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return Ceilings{Policy: p}
}

func TestStageBShapeAdmission(t *testing.T) {
	c := ceilingsFor(t, bPolicy)
	for _, n := range []int{1, 2, 3} {
		team, launch := bTeam(n)
		if err := c.AdmitFormation(context.Background(), svcPrincipal(), teamsvc.FormRequest{Key: "ok", Team: team, Launch: launch}); err != nil {
			t.Fatalf("%d workers refused: %v", n, err)
		}
	}
	// Orchestrator and reviewer on Astra, workers on Sol: a different model on
	// one runtime satisfies the reviewer rule.
	if got, ok := c.Policy.PairingFor("ok"); !ok || !got.Different || got.WorkerModel != "sol" || got.ReviewerModel != "astra" {
		t.Fatalf("pairing %+v", got)
	}
	cases := map[string]func(*teams.Team, *teams.LaunchRequest){
		"four workers": func(tm *teams.Team, l *teams.LaunchRequest) {
			*tm, *l = bTeam(4)
		},
		"workers over their count": func(_ *teams.Team, l *teams.LaunchRequest) { l.Counts["workers"] = 4 },
		"no worker":                func(_ *teams.Team, l *teams.LaunchRequest) { l.Counts["workers"] = 0 },
		"two orchestrators":        func(tm *teams.Team, _ *teams.LaunchRequest) { tm.Slots[1].Max = 2 },
		"no reviewer":              func(tm *teams.Team, _ *teams.LaunchRequest) { tm.Slots[3].Min = 0 },
		"pooled orchestrator": func(tm *teams.Team, _ *teams.LaunchRequest) {
			tm.Slots[1].Resolution, tm.Slots[1].Pool, tm.Slots[1].Activation = teams.Pool, "p", teams.Concurrent
			tm.Slots[1].Identities = []mesh.URN{"msg://agent/local/o"}
		},
		"agent seat in the owner slot": func(tm *teams.Team, l *teams.LaunchRequest) { tm.Slots[0].Min = 1; l.Counts["coordinator"] = 1 },
		"extra slot": func(tm *teams.Team, _ *teams.LaunchRequest) {
			tm.Slots = append(tm.Slots, teams.Slot{Name: "extra", Definition: astra, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 0, Max: 1})
		},
		"owner slot is not coordinator": func(tm *teams.Team, _ *teams.LaunchRequest) { tm.Phases[0].OwnerSlot = "orchestrator" },
		"unlisted definition": func(tm *teams.Team, _ *teams.LaunchRequest) {
			tm.Slots[1].Definition = mesh.DefinitionRef{ID: "x", Revision: "r", Digest: "d"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			team, launch := bTeam(2)
			mutate(&team, &launch)
			err := c.AdmitFormation(context.Background(), svcPrincipal(), teamsvc.FormRequest{Key: "bad", Team: team, Launch: launch})
			if !errors.Is(err, teamsvc.ErrDenied) {
				t.Fatalf("want denied, got %v", err)
			}
		})
	}
}

func TestStageBGrantsCannotLetWorkersReachAnyoneButTheOrchestrator(t *testing.T) {
	c := ceilingsFor(t, bPolicy)
	for name, g := range map[string]teams.Grant{
		"worker to worker":      {FromSlot: "workers", Verb: teams.MayMessage, ToSlot: "workers"},
		"worker to self slot":   {FromSlot: "workers", Verb: teams.MayMessage, ToSlot: teams.Self},
		"worker to reviewer":    {FromSlot: "workers", Verb: teams.MayMessage, ToSlot: "reviewer"},
		"reviewer to worker":    {FromSlot: "reviewer", Verb: teams.MayMessage, ToSlot: "workers"},
		"worker to owner":       {FromSlot: "workers", Verb: teams.MayMessage, ToSlot: "coordinator"},
		"worker to actor":       {FromSlot: "workers", Verb: teams.MayMessage, ToActor: "msg://agent/local/anyone"},
		"actor to worker":       {FromActor: "msg://agent/local/anyone", Verb: teams.MayMessage, ToSlot: "workers"},
		"worker assigns":        {FromSlot: "workers", Verb: teams.MayAssign, ToSlot: "orchestrator"},
		"worker delegates":      {FromSlot: "workers", Verb: teams.MayDelegate, ToSlot: "orchestrator"},
		"reviewer delegates":    {FromSlot: "reviewer", Verb: teams.MayDelegate, ToSlot: "orchestrator"},
		"owner to worker":       {FromSlot: "coordinator", Verb: teams.MayMessage, ToSlot: "workers"},
		"orchestrator spawns":   {FromSlot: "orchestrator", Verb: teams.MaySpawn, ToSlot: "workers"},
		"orchestrator admins":   {FromSlot: "orchestrator", Verb: teams.MayAdmin, ToSlot: "workers"},
		"workers to orch spawn": {FromSlot: "workers", Verb: teams.MaySpawn, ToSlot: "orchestrator"},
	} {
		t.Run(name, func(t *testing.T) {
			team, launch := bTeam(2)
			team.Authority.Grants = append(team.Authority.Grants, g)
			err := c.AdmitFormation(context.Background(), svcPrincipal(), teamsvc.FormRequest{Key: "grant", Team: team, Launch: launch})
			if !errors.Is(err, teamsvc.ErrDenied) {
				t.Fatalf("grant admitted: %v", err)
			}
		})
	}
}

func TestServiceGateAndLifecycle(t *testing.T) {
	c := ceilingsFor(t, bPolicy)
	ctx := context.Background()
	if _, err := c.FormationCeilings(ctx, svcPrincipal()); err != nil {
		t.Fatalf("listed service refused: %v", err)
	}
	for name, mutate := range map[string]func(*teamsvc.Principal){
		"unlisted":   func(p *teamsvc.Principal) { p.ID = "msg://service/torque/other" },
		"unverified": func(p *teamsvc.Principal) { p.Verified = false },
		"as agent":   func(p *teamsvc.Principal) { p.Kind = mesh.ActorAgent },
		"as user":    func(p *teamsvc.Principal) { p.Kind = mesh.ActorUser },
	} {
		p := svcPrincipal()
		mutate(&p)
		if _, err := c.FormationCeilings(ctx, p); !errors.Is(err, teamsvc.ErrDenied) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// No services listed: a verified service is refused, the default.
	none := ceilingsFor(t, validPolicy)
	if _, err := none.FormationCeilings(ctx, svcPrincipal()); !errors.Is(err, teamsvc.ErrDenied) {
		t.Fatalf("service admitted without a services list: %v", err)
	}
	if c.Policy.HostOptions().ActorTrust[dispatcherURN] != teamhost.TrustTrusted {
		t.Fatal("listed service is not a trusted actor for the team host")
	}
	team, _ := bTeam(2)
	if !c.DissolvesOnResult(team, "orchestrator") || c.DissolvesOnResult(team, "workers") || c.DissolvesOnResult(team, "reviewer") {
		t.Fatal("only the orchestrator's result is final")
	}
	stageA := team
	stageA.Slots = []teams.Slot{team.Slots[0], team.Slots[2], team.Slots[3]}
	if c.DissolvesOnResult(stageA, "orchestrator") {
		t.Fatal("a stage A team has no orchestrator to end it")
	}
}

func TestStageBPairingPreference(t *testing.T) {
	sameModel := func(tm *teams.Team) { tm.Slots[2].Definition, tm.Slots[3].Definition = astra, astra }
	team, launch := bTeam(2)
	sameModel(&team)
	req := teamsvc.FormRequest{Key: "same", Team: team, Launch: launch}
	// Another launch could separate them, so the default requires it.
	if err := ceilingsFor(t, bPolicy).AdmitFormation(context.Background(), svcPrincipal(), req); !errors.Is(err, teamsvc.ErrDenied) {
		t.Fatalf("same model accepted though the allowlist can separate them: %v", err)
	}
	// "prefer" makes it a preference.
	if err := ceilingsFor(t, strings.Replace(bPolicy, "services:", "reviewer_pairing: prefer\nservices:", 1)).AdmitFormation(context.Background(), svcPrincipal(), req); err != nil {
		t.Fatalf("prefer mode refused: %v", err)
	}
	// An allowlist that cannot separate them (one launch) never refuses.
	one := "version: 1\nservices: [" + dispatcherURN + "]\nlaunches:\n  - {launch: la, runtime: claude, model: astra, definition: {id: claude-astra, revision: r1, digest: d-astra}}\n"
	if err := ceilingsFor(t, one).AdmitFormation(context.Background(), svcPrincipal(), req); err != nil {
		t.Fatalf("single-launch allowlist refused: %v", err)
	}
	// Different runtime for the reviewer (Codex) satisfies it as well.
	team, launch = bTeam(2)
	team.Slots[3].Definition = codex
	if err := ceilingsFor(t, bPolicy).AdmitFormation(context.Background(), svcPrincipal(), teamsvc.FormRequest{Key: "rt", Team: team, Launch: launch}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyCeilingsAndServicesParse(t *testing.T) {
	p, err := Parse([]byte(bPolicy))
	if err != nil {
		t.Fatal(err)
	}
	hi, def := p.MaxLimits(), p.DefaultLimits()
	if hi.Timeout < 6*time.Hour || hi.MaxRounds < 100 || hi.MaxStalls < 20 || hi.MaxDepth < 2 || hi.Budget <= 1 {
		t.Fatalf("built-in maximum is not generous: %+v", hi)
	}
	if def.Timeout < 2*time.Hour || def.MaxRounds < 100 || def.MaxStalls != 20 || def.MaxDepth != 2 || def.Timeout > hi.Timeout || def.MaxRounds > hi.MaxRounds {
		t.Fatalf("built-in defaults: %+v", def)
	}
	if got := p.Services(); len(got) != 1 || got[0] != dispatcherURN || !p.IsService(dispatcherURN) || p.IsService("msg://service/x/y") {
		t.Fatalf("services %v", got)
	}
	custom := bPolicy + "ceilings:\n  default: {timeout: 2h, max_rounds: 50}\n  max: {timeout: 3h, max_rounds: 60, budget: 5}\n"
	p, err = Parse([]byte(custom))
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxLimits().Timeout != 3*time.Hour || p.MaxLimits().Budget != 5 || p.DefaultLimits().Timeout != 2*time.Hour || p.DefaultLimits().MaxRounds != 50 {
		t.Fatalf("custom ceilings: %+v %+v", p.MaxLimits(), p.DefaultLimits())
	}
	// A default the file leaves unset is clamped to a lowered maximum.
	p, err = Parse([]byte(bPolicy + "ceilings:\n  max: {timeout: 1h, budget: 5}\n"))
	if err != nil || p.DefaultLimits().Timeout != time.Hour || p.DefaultLimits().Budget != 5 {
		t.Fatalf("clamp: %v %+v", err, p.DefaultLimits())
	}
	for name, tc := range map[string]struct{ raw, want string }{
		"default over max":  {bPolicy + "ceilings:\n  default: {timeout: 20h}\n", "default timeout exceeds max timeout"},
		"bad service":       {strings.Replace(bPolicy, dispatcherURN, "dispatcher", 1), "service principal urn"},
		"user as service":   {strings.Replace(bPolicy, dispatcherURN, "msg://user/local/operator", 1), "service principal urn"},
		"duplicate service": {strings.Replace(bPolicy, "services: ["+dispatcherURN+"]", "services: ["+dispatcherURN+", "+dispatcherURN+"]", 1), "duplicate service"},
		"bad pairing":       {bPolicy + "reviewer_pairing: sometimes\n", "reviewer_pairing"},
		"unknown ceiling":   {bPolicy + "ceilings:\n  max: {hours: 3}\n", "hours"},
		"negative":          {bPolicy + "ceilings:\n  max: {max_rounds: -1}\n", "max"},
	} {
		if _, err := Parse([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}

func stageATeamFor(workers, reviewer mesh.DefinitionRef) teams.Team {
	fresh := func(name string, pin mesh.DefinitionRef, lo, hi int) teams.Slot {
		return teams.Slot{Name: name, Definition: pin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: lo, Max: hi}
	}
	return teams.Team{ID: "a", Name: "A", Version: 1,
		Slots:     []teams.Slot{fresh("coordinator", astra, 0, 1), fresh("workers", workers, 1, 1), fresh("reviewer", reviewer, 1, 1)},
		Phases:    []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"coordinator", "workers", "reviewer"}, OwnerSlot: "coordinator", ExitTrigger: teams.Trigger{Kind: "manual", Spec: map[string]string{"signal": "done"}}}},
		Authority: teams.Authority{Mode: teams.Strict, Grants: []teams.Grant{{FromSlot: "coordinator", Verb: teams.MayMessage, ToSlot: "workers"}}},
		Routing:   teams.Routing{CoordinatorSlot: "coordinator"}}
}

// Stage A teams keep the conservative ceilings, the original pairing rule and
// no reaper; only a team with an orchestrator slot gets the Stage B behavior.
func TestStageAIsUnchangedByStageB(t *testing.T) {
	c := ceilingsFor(t, bPolicy)
	ctx := context.Background()
	a := teamsvc.FormRequest{Key: "a", Team: stageATeamFor(sol, codex)}
	got, err := c.FormationCeilings(ctx, svcPrincipal())
	if err != nil || got.Limits.MaxDepth != 1 || got.MaxSlots != 3 || got.Limits.Timeout != 5*time.Minute || got.DefaultLimits != (mesh.Limits{}) {
		t.Fatalf("stage A ceilings: %+v %v", got, err)
	}
	if got, err = c.FormationCeilingsFor(ctx, svcPrincipal(), a); err != nil || got.Limits.MaxDepth != 1 || got.Limits.Timeout != 5*time.Minute {
		t.Fatalf("stage A request ceilings: %+v %v", got, err)
	}
	b, launch := bTeam(2)
	if got, err = c.FormationCeilingsFor(ctx, svcPrincipal(), teamsvc.FormRequest{Key: "b", Team: b, Launch: launch}); err != nil || got.Limits.MaxDepth != 3 || got.DefaultLimits.Timeout != 4*time.Hour {
		t.Fatalf("stage B ceilings: %+v %v", got, err)
	}
	if c.ManagesLifecycle(a.Team) || !c.ManagesLifecycle(b) {
		t.Fatal("only stage B runs are reaped")
	}
	// The original rule: two runtimes allowlisted, same runtime and model is
	// refused, and reviewer_pairing: prefer does not relax it for stage A.
	same := teamsvc.FormRequest{Key: "same", Team: stageATeamFor(astra, astra)}
	for _, raw := range []string{bPolicy, strings.Replace(bPolicy, "services:", "reviewer_pairing: prefer\nservices:", 1)} {
		if err := ceilingsFor(t, raw).AdmitFormation(ctx, svcPrincipal(), same); !errors.Is(err, teamsvc.ErrDenied) {
			t.Fatalf("stage A same-runtime reviewer accepted: %v", err)
		}
	}
	// Same runtime, different model: stage A (as before) accepts it.
	if err := c.AdmitFormation(ctx, svcPrincipal(), teamsvc.FormRequest{Key: "model", Team: stageATeamFor(sol, astra)}); err != nil {
		t.Fatal(err)
	}
}

func TestCeilingTimeoutMustBeADuration(t *testing.T) {
	for _, raw := range []string{"ceilings:\n  max: {timeout: 14400}\n", "ceilings:\n  default: {timeout: 3600}\n"} {
		_, err := Parse([]byte(bPolicy + raw))
		if err == nil || !strings.Contains(err.Error(), "duration string such as 4h") {
			t.Fatalf("bare number accepted or unclear error: %v", err)
		}
	}
	if _, err := Parse([]byte(bPolicy + "ceilings:\n  max: {timeout: 1ns}\n")); err == nil || !strings.Contains(err.Error(), "use a duration such as 4h") {
		t.Fatalf("sub-second duration accepted: %v", err)
	}
	if _, err := Parse([]byte(bPolicy + "ceilings:\n  max: {timeout: 4h}\n")); err != nil {
		t.Fatal(err)
	}
}

func TestAdmitMemberAddCaps(t *testing.T) {
	c := ceilingsFor(t, bPolicy)
	team, _ := bTeam(3)
	member := func(slot, status string) teams.Member {
		return teams.Member{ID: slot + status, Slot: slot, Status: status, Governance: teams.MemberRole}
	}
	owner := teams.Member{ID: "owner", Slot: "coordinator", Status: "active", Governance: teams.Owner}
	base := []teams.Member{owner, member("orchestrator", "active"), member("reviewer", "active"), member("workers", "active")}
	if err := c.AdmitMemberAdd(team, teams.Roster{Members: base}, "workers"); err != nil {
		t.Fatalf("add to a small team refused: %v", err)
	}
	three := append(append([]teams.Member{}, base...), member("workers", "provisioning"), member("workers", "stopping"))
	for name, tc := range map[string]struct {
		members []teams.Member
		slot    string
	}{
		"fourth worker":     {three, "workers"},
		"not a worker slot": {base, "reviewer"},
		"sixth agent":       {append(append([]teams.Member{}, base...), member("workers", "active"), member("workers", "active"), member("orchestrator", "active")), "workers"},
	} {
		if err := c.AdmitMemberAdd(team, teams.Roster{Members: tc.members}, tc.slot); !errors.Is(err, teamsvc.ErrDenied) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A stopped worker frees its seat; a Stage A team is not constrained here.
	freed := append(append([]teams.Member{}, base...), member("workers", "active"), member("workers", "stopped"))
	if err := c.AdmitMemberAdd(team, teams.Roster{Members: freed}, "workers"); err != nil {
		t.Fatalf("stopped member still counted: %v", err)
	}
	if err := c.AdmitMemberAdd(stageATeamFor(sol, codex), teams.Roster{}, "reviewer"); err != nil {
		t.Fatalf("stage A add constrained: %v", err)
	}
}
