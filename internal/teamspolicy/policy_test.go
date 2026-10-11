package teamspolicy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

const validPolicy = `version: 1
launches:
  - launch: tether-claude
    runtime: claude
    definition: {id: claude-agent, revision: r1, digest: d-claude}
  - launch: agent-mux-codex-app-server
    runtime: codex
    definition: {id: codex-agent, revision: r1, digest: d-codex}
`

func operator() teamsvc.Principal {
	return teamsvc.Principal{ID: teamsvc.LocalOperator, Kind: mesh.ActorUser, Verified: true, LocalOperator: true}
}

func TestLoadAbsentFileDeniesEverything(t *testing.T) {
	p, err := Load(t.TempDir())
	if err != nil || p != nil {
		t.Fatalf("absent file: %v %v", p, err)
	}
	if !p.Paused() || len(p.Launches()) != 0 || len(p.LaunchTargets()) != 0 || len(p.HostOptions().TrustTiers) != 0 {
		t.Fatal("nil policy grants something")
	}
	if _, err = (Ceilings{Policy: p}).FormationCeilings(context.Background(), operator()); !errors.Is(err, teamsvc.ErrDenied) {
		t.Fatalf("operator formed without a policy: %v", err)
	}
}

func TestParseRejectsInvalidPolicyClearly(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"version":   {"launches: []\n", "version must be 1"},
		"empty":     {"version: 1\n", "at least one exact launch"},
		"pattern":   {"version: 1\nlaunches:\n  - {launch: 'tether-*', runtime: claude, definition: {id: a, revision: r, digest: d}}\n", "no patterns"},
		"runtime":   {"version: 1\nlaunches:\n  - {launch: a, definition: {id: a, revision: r, digest: d}}\n", "runtime is required"},
		"no digest": {"version: 1\nlaunches:\n  - {launch: a, runtime: x, definition: {id: a, revision: r}}\n", "exact pin"},
		"dup":       {validPolicy + "  - {launch: tether-claude, runtime: claude, definition: {id: z, revision: r, digest: d}}\n", "duplicate launch"},
		"dup pin":   {validPolicy + "  - {launch: other, runtime: claude, definition: {id: claude-agent, revision: r1, digest: d-claude}}\n", "already bound"},
		"unknown":   {validPolicy + "paused: true\n", "field paused not found"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	root := t.TempDir()
	if err := os.WriteFile(Path(root), []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), FileName) {
		t.Fatalf("load error does not name the file: %v", err)
	}
}

func TestHostOptionsTrustOnlyAllowlistAndOperator(t *testing.T) {
	p, err := Parse([]byte(validPolicy))
	if err != nil {
		t.Fatal(err)
	}
	opts := p.HostOptions()
	if len(opts.TrustTiers) != 2 || opts.TrustTiers[mesh.DefinitionRef{ID: "claude-agent", Revision: "r1", Digest: "d-claude"}] != teamhost.TrustTrusted {
		t.Fatalf("tiers: %v", opts.TrustTiers)
	}
	if len(opts.ActorTrust) != 1 || opts.ActorTrust[teamsvc.LocalOperator] != teamhost.TrustTrusted {
		t.Fatalf("actors: %v", opts.ActorTrust)
	}
	if p.LaunchTargets()[mesh.DefinitionRef{ID: "codex-agent", Revision: "r1", Digest: "d-codex"}] != "agent-mux-codex-app-server" {
		t.Fatal("pin target")
	}
	if !KnownCapability(mesh.SpawnCapabilityURI) || KnownCapability("urn:other") {
		t.Fatal("capability vocabulary")
	}
}

func TestCeilingsGateOperatorAndPause(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(Path(root), []byte(validPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	c := Ceilings{Policy: p}
	got, err := c.FormationCeilings(context.Background(), operator())
	if err != nil || got.Limits.MaxDepth != 1 || got.MaxSlots != 3 || len(got.AllowedPermissions) != 1 || got.AllowedPermissions[0] != teams.MayMessage {
		t.Fatalf("stage A ceilings: %+v %v", got, err)
	}
	for name, mutate := range map[string]func(*teamsvc.Principal){
		"unverified": func(p *teamsvc.Principal) { p.Verified = false },
		"no socket":  func(p *teamsvc.Principal) { p.LocalOperator = false },
		"agent":      func(p *teamsvc.Principal) { p.Kind = mesh.ActorAgent },
		"service": func(p *teamsvc.Principal) {
			p.ID, p.Kind, p.LocalOperator = "msg://service/local/x", mesh.ActorService, false
		},
	} {
		pr := operator()
		mutate(&pr)
		if _, err := c.FormationCeilings(context.Background(), pr); !errors.Is(err, teamsvc.ErrDenied) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Pause by marker file, no restart; resume by removing it.
	marker := filepath.Join(root, PauseFileName)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FormationCeilings(context.Background(), operator()); !errors.Is(err, teamsvc.ErrUnavailable) {
		t.Fatalf("marker pause: %v", err)
	}
	_ = os.Remove(marker)
	p.Pause()
	if _, err := c.FormationCeilings(context.Background(), operator()); !errors.Is(err, teamsvc.ErrUnavailable) {
		t.Fatalf("in-memory pause: %v", err)
	}
	p.Resume()
	if _, err := c.FormationCeilings(context.Background(), operator()); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

func admitTeam() teams.Team {
	claude := mesh.DefinitionRef{ID: "claude-agent", Revision: "r1", Digest: "d-claude"}
	codex := mesh.DefinitionRef{ID: "codex-agent", Revision: "r1", Digest: "d-codex"}
	slot := func(name string, def mesh.DefinitionRef, minimum int) teams.Slot {
		return teams.Slot{Name: name, Definition: def, Resolution: teams.Fresh, Activation: teams.Singleton, Min: minimum, Max: 1}
	}
	return teams.Team{Slots: []teams.Slot{slot(SlotCoordinator, claude, 0), slot(SlotWorkers, claude, 1), slot(SlotReviewer, codex, 1)}, Routing: teams.Routing{CoordinatorSlot: SlotCoordinator}}
}

func TestAdmitFormationReasons(t *testing.T) {
	p, err := Parse([]byte(validPolicy))
	if err != nil {
		t.Fatal(err)
	}
	c := Ceilings{Policy: p}
	rogue := mesh.DefinitionRef{ID: "rogue", Revision: "r1", Digest: "d"}
	for name, tc := range map[string]struct {
		mutate func(*teams.Team)
		want   string
	}{
		"valid":           {func(*teams.Team) {}, ""},
		"extra slot":      {func(t *teams.Team) { t.Slots = append(t.Slots, t.Slots[0]) }, "exactly three slots"},
		"renamed slot":    {func(t *teams.Team) { t.Slots[1].Name = "grunts" }, "missing slot"},
		"not allowlisted": {func(t *teams.Team) { t.Slots[1].Definition = rogue }, "allowlisted exact pin"},
		"same runtime":    {func(t *teams.Team) { t.Slots[2].Definition = t.Slots[1].Definition }, "different runtime"},
		"durable":         {func(t *teams.Team) { t.Slots[2].Resolution = teams.Durable }, "fresh members"},
		"coordinator":     {func(t *teams.Team) { t.Routing.CoordinatorSlot = SlotWorkers }, "coordinator slot must be"},
		"workers min 0":   {func(t *teams.Team) { t.Slots[1].Min = 0 }, "size must be"},
		"workers max 3":   {func(t *teams.Team) { t.Slots[1].Max = 3 }, "size must be"},
	} {
		t.Run(name, func(t *testing.T) {
			team := admitTeam()
			tc.mutate(&team)
			err := c.AdmitFormation(context.Background(), operator(), teamsvc.FormRequest{Team: team})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, teamsvc.ErrDenied) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want denied %q, got %v", tc.want, err)
			}
		})
	}
	// With only one runtime allowlisted the pairing is a preference, not a rule.
	single, _ := Parse([]byte("version: 1\nlaunches:\n  - {launch: tether-claude, runtime: claude, definition: {id: claude-agent, revision: r1, digest: d-claude}}\n"))
	team := admitTeam()
	team.Slots[2].Definition = team.Slots[1].Definition
	if err := (Ceilings{Policy: single}).AdmitFormation(context.Background(), operator(), teamsvc.FormRequest{Team: team}); err != nil {
		t.Fatal(err)
	}
}
