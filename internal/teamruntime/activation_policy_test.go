package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/agentdef"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamspolicy"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

var (
	claudePin = mesh.DefinitionRef{ID: "claude-agent", Revision: "r1", Digest: "d-claude"}
	codexPin  = mesh.DefinitionRef{ID: "codex-agent", Revision: "r1", Digest: "d-codex"}
	rogue     = mesh.DefinitionRef{ID: "rogue", Revision: "r1", Digest: "d-rogue"}
)

const stageAPolicy = `version: 1
launches:
  - {launch: tether-claude, runtime: claude, definition: {id: claude-agent, revision: r1, digest: d-claude}}
  - {launch: agent-mux-codex-app-server, runtime: codex, definition: {id: codex-agent, revision: r1, digest: d-codex}}
`

type pinnedDefinitions struct{}

func (pinnedDefinitions) Load(_ context.Context, p mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
	if p != claudePin && p != codexPin {
		return definitionresolve.VerifiedDefinition{}, definitionresolve.ErrPinMismatch
	}
	return definitionresolve.VerifiedDefinition{Pin: p, Definition: &agentdef.Definition{Name: p.ID}}, nil
}

type policyFixture struct {
	reg      *registry.Service
	svc      *teamsvc.Service
	policy   *teamspolicy.Policy
	effects  *sessionEffects
	sessions *Sessions
}

func operatorContext(t *testing.T) context.Context {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join("/tmp", "tp-"+filepath.Base(t.TempDir())+".sock"))
	check(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := listener.Accept(); accepted <- c }()
	client, err := net.Dial("unix", listener.Addr().String())
	check(t, err)
	t.Cleanup(func() { _ = client.Close() })
	conn := <-accepted
	t.Cleanup(func() { _ = conn.Close() })
	return identity.WithPrincipal(identity.ConnectionContext(ctx, conn), identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"})
}

func stageATeam() teams.Team {
	return teams.Team{ID: "stage-a", Name: "Stage A", Version: 1, Slots: []teams.Slot{
		{Name: "coordinator", Definition: claudePin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 0, Max: 1},
		{Name: "workers", Definition: claudePin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 1, Max: 1},
		{Name: "reviewer", Definition: codexPin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 1, Max: 1},
	}, Phases: []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"coordinator", "workers", "reviewer"}, OwnerSlot: "coordinator", ExitTrigger: teams.Trigger{Kind: "manual", Spec: map[string]string{"signal": "done"}}}},
		Authority: teams.Authority{Mode: teams.Strict, Grants: []teams.Grant{{FromSlot: "coordinator", Verb: teams.MayMessage, ToSlot: "workers"}, {FromSlot: "workers", Verb: teams.MayMessage, ToSlot: "reviewer"}}},
		Policy:    teams.Policy{Spawn: teamspolicy.StageA().Limits, History: mesh.HistoryNone}, Routing: teams.Routing{CoordinatorSlot: "coordinator"}}
}

func newPolicyFixture(t *testing.T, raw string) (*policyFixture, func(string) int) {
	t.Helper()
	root := t.TempDir()
	if raw != "" {
		check(t, writeFile(teamspolicy.Path(root), raw))
	}
	policy, err := teamspolicy.Load(root)
	check(t, err)
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	targets := map[mesh.DefinitionRef]LaunchTarget{}
	for pin, launch := range policy.LaunchTargets() {
		targets[pin] = LaunchTarget{LaunchID: launch}
	}
	e, err := NewLegacyEnroller(db.DB(), s, reg, pinnedDefinitions{}, targets)
	check(t, err)
	effects := &sessionEffects{db: db}
	sessions, err := NewSessions(db, s, effects, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	host, err := teamhost.New(db.DB(), s, teamhost.Ports{Enroller: e, Sessions: sessions, Messenger: messages, Channels: Channels{}}, policy.HostOptions())
	check(t, err)
	service, err := teamsvc.New(teamsvc.Deps{Ceilings: teamspolicy.Ceilings{Policy: policy}, Principals: Principals{Mode: identity.Enforce, Sessions: e}, Runs: host, Calls: host, Definitions: s, Roster: s, Ledger: s, Signals: s, Provisioner: host, Workflows: host, Triggers: host, Sender: host, Routing: host, Trust: host, Approvals: host, Clock: host, IDs: host, Defaults: teamsvc.ConservativePolicy().Limits})
	check(t, err)
	count := func(table string) int {
		var n int
		check(t, db.DB().QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		return n
	}
	return &policyFixture{reg: reg, svc: service, policy: policy, effects: effects, sessions: sessions}, count
}

// poolWorkers is n distinct (unenrolled) agent references; fine for denial cases.
func poolWorkers(n int) teams.Slot {
	ids := make([]mesh.URN, n)
	for i := range ids {
		ids[i] = mesh.URN("msg://agent/local/pool-" + string(rune('a'+i)))
	}
	return teams.Slot{Name: "workers", Definition: claudePin, Resolution: teams.Pool, Pool: "workers", Identities: ids, Activation: teams.Concurrent, Min: 1, Max: n}
}

func TestStageAFormationEndToEndWithFakeLauncher(t *testing.T) {
	f, count := newPolicyFixture(t, stageAPolicy)
	op := operatorContext(t)
	team := stageATeam()
	raw, err := json.Marshal(claudePin)
	check(t, err)
	var ids []mesh.URN
	for _, name := range []string{"w1", "w2"} {
		profile, _, err := f.reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: name, Props: map[string]string{"team_definition": string(raw)}}, "fixture", name)
		check(t, err)
		ids = append(ids, mesh.URN(profile.URN))
	}
	team.Slots[1] = teams.Slot{Name: "workers", Definition: claudePin, Resolution: teams.Pool, Pool: "workers", Identities: ids, Activation: teams.Concurrent, Min: 1, Max: 2}
	result, err := f.svc.Form(op, teamsvc.FormRequest{Key: "form", Team: team, Launch: teams.LaunchRequest{Counts: map[string]int{"workers": 2}}})
	check(t, err)
	if result.Run == nil || f.effects.launched != 3 {
		t.Fatalf("expected 2 pool workers + reviewer launched, got %d (%+v)", f.effects.launched, result.Run)
	}
	var sessionsByLaunch = map[string]int{}
	rows, err := (&sessionEffects{db: f.effects.db}).db.DB().Query(`SELECT launch_id, count(*) FROM sessions GROUP BY launch_id`)
	check(t, err)
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		check(t, rows.Scan(&id, &n))
		sessionsByLaunch[id] = n
	}
	if sessionsByLaunch["tether-claude"] != 2 || sessionsByLaunch["agent-mux-codex-app-server"] != 1 {
		t.Fatalf("sessions were not launched through the pinned launch ids: %v", sessionsByLaunch)
	}
	if count("team_runs") != 1 {
		t.Fatal("run missing")
	}
	// Admission pause refuses a second formation without a restart, then resumes.
	f.policy.Pause()
	second := stageATeam()
	second.ID, second.Name = "stage-a-2", "Second"
	if _, err = f.svc.Form(op, teamsvc.FormRequest{Key: "paused", Team: second}); !errors.Is(err, teamsvc.ErrUnavailable) || count("team_runs") != 1 {
		t.Fatalf("pause did not refuse formation: %v", err)
	}
	f.policy.Resume()
	if _, err = f.svc.Form(op, teamsvc.FormRequest{Key: "resumed", Team: second}); err != nil {
		t.Fatalf("formation after resume: %v", err)
	}
}

func TestStageAFormationDenialsHaveNoSideEffects(t *testing.T) {
	cases := map[string]struct {
		raw    string
		mutate func(*teams.Team)
		ctx    func(*testing.T) context.Context
		want   error
	}{
		"no policy file": {raw: "", want: teamsvc.ErrDenied},
		"non-operator": {raw: stageAPolicy, ctx: func(t *testing.T) context.Context {
			return identity.WithPrincipal(operatorContext(t), identity.Principal{Verified: true, ID: "msg://service/local/other", Kind: "service"})
		}, want: teamsvc.ErrDenied},
		"too many slots": {raw: stageAPolicy, mutate: func(tm *teams.Team) {
			tm.Slots = append(tm.Slots, teams.Slot{Name: "extra", Definition: claudePin, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 0, Max: 1})
			tm.Phases[0].ActiveSlots = append(tm.Phases[0].ActiveSlots, "extra")
		}, want: teamsvc.ErrDenied},
		"too many workers": {raw: stageAPolicy, mutate: func(tm *teams.Team) { tm.Slots[1] = poolWorkers(3) }, want: teamsvc.ErrDenied},
		"disallowed permission": {raw: stageAPolicy, mutate: func(tm *teams.Team) {
			tm.Authority.Grants = append(tm.Authority.Grants, teams.Grant{FromSlot: "coordinator", Verb: teams.MaySpawn, ToSlot: "workers"})
		}, want: teamsvc.ErrDenied},
		"non-allowlisted launch": {raw: stageAPolicy, mutate: func(tm *teams.Team) { tm.Slots[1].Definition = rogue }, want: teamsvc.ErrDenied},
		"same-runtime reviewer":  {raw: stageAPolicy, mutate: func(tm *teams.Team) { tm.Slots[2].Definition = claudePin }, want: teamsvc.ErrDenied},
		"depth above ceiling":    {raw: stageAPolicy, mutate: func(tm *teams.Team) { tm.Policy.Spawn.MaxDepth = 2 }, want: nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, count := newPolicyFixture(t, tc.raw)
			team := stageATeam()
			if tc.mutate != nil {
				tc.mutate(&team)
			}
			c := operatorContext(t)
			if tc.ctx != nil {
				c = tc.ctx(t)
			}
			_, err := f.svc.Form(c, teamsvc.FormRequest{Key: "denied", Team: team})
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			for _, table := range []string{"team_definitions", "team_runs", "team_port_intents", "registry_entries", "runtime_bindings", "sessions"} {
				if n := count(table); n != 0 {
					t.Fatalf("denied formation left %d rows in %s", n, table)
				}
			}
			if f.effects.launched != 0 {
				t.Fatal("denied formation launched a session")
			}
		})
	}
}

func TestStageASingleRuntimeAllowlistMakesDifferentReviewerOptional(t *testing.T) {
	single := `version: 1
launches:
  - {launch: tether-claude, runtime: claude, definition: {id: claude-agent, revision: r1, digest: d-claude}}
`
	f, _ := newPolicyFixture(t, single)
	team := stageATeam()
	team.Slots[2].Definition = claudePin
	if _, err := f.svc.Form(operatorContext(t), teamsvc.FormRequest{Key: "single", Team: team}); err != nil {
		t.Fatalf("same-runtime reviewer must be allowed when no other runtime is allowlisted: %v", err)
	}
	if got, ok := f.policy.PairingFor("single"); !ok || got.Different || got.WorkerRuntime != "claude" || got.ReviewerRuntime != "claude" {
		t.Fatalf("fallback not recorded: %+v", got)
	}
}

func TestStageAReviewerDifferentRuntimeOrModelIsRecorded(t *testing.T) {
	raw := `version: 1
launches:
  - {launch: tether-claude, runtime: claude, model: opus, definition: {id: claude-agent, revision: r1, digest: d-claude}}
  - {launch: tether-codex, runtime: codex, model: gpt, definition: {id: codex-agent, revision: r1, digest: d-codex}}
`
	f, _ := newPolicyFixture(t, raw)
	team := stageATeam()
	team.Slots[2].Definition = codexPin
	if _, err := f.svc.Form(operatorContext(t), teamsvc.FormRequest{Key: "pair", Team: team}); err != nil {
		t.Fatal(err)
	}
	got, ok := f.policy.PairingFor("pair")
	if !ok || !got.Different || got.WorkerRuntime != "claude" || got.WorkerModel != "opus" || got.ReviewerRuntime != "codex" || got.ReviewerModel != "gpt" {
		t.Fatalf("pairing not recorded: %+v", got)
	}
	team = stageATeam()
	team.Slots[2].Definition = claudePin
	if _, err := f.svc.Form(operatorContext(t), teamsvc.FormRequest{Key: "same", Team: team}); err == nil {
		t.Fatal("same runtime and model reviewer accepted with two runtimes allowlisted")
	}
}

func writeFile(path, raw string) error { return os.WriteFile(path, []byte(raw), 0o600) }
