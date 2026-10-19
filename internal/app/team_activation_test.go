package app

import (
	"encoding/json"
	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/harness/core/sandbox"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/teamactivation"
	"strings"
	"testing"
)

func TestTeamActivationLaunchFingerprintTracksCatalogBehavior(t *testing.T) {
	svc, write := newLaunchCatalogService(t)
	before, err := svc.InspectTeamLaunch(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.InspectTeamLaunch(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	if before.Fingerprint != again.Fingerprint {
		t.Fatal("unchanged launch fingerprint is unstable")
	}
	write("launches/first.yaml", "id: first\nproject: p\nagent: a\nprovider: cli\noverrides:\n  env:\n    PRIVATE_FIXTURE: SYNTHETIC_SECRET\n")
	after, err := svc.InspectTeamLaunch(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	if before.Fingerprint == after.Fingerprint {
		t.Fatal("unchanged pin could conceal catalog behavior change")
	}
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SYNTHETIC_SECRET") {
		t.Fatal("inspection exposes environment")
	}
	if after.CatalogAgent != "a" || after.Project != "p" || after.Provider != "cli" {
		t.Fatal("wrong launch mapping")
	}
}

func TestTeamActivationProjectionExcludesValuesAndSealsExactPlan(t *testing.T) {
	rig := newCodexRig(t)
	created, err := rig.svc.CreateTeamSession(t.Context(), "evidence-create", "codex-launch")
	if err != nil {
		t.Fatal(err)
	}
	p, err := rig.svc.Store.GetLaunchPlan(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if p.ActivationEvidence == nil {
		t.Fatal("created plan missing evidence")
	}
	before := p.ActivationEvidence.Fingerprint
	p.Env = map[string]string{"SAFE_KEY": "SYNTHETIC_SECRET"}
	p.Args = []string{"SYNTHETIC_ARG"}
	p.BootPrompt = "SYNTHETIC_PROMPT"
	first, err := rig.svc.teamPlanEvidence(p, "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Env["SAFE_KEY"] = "DIFFERENT_SECRET"
	p.Args = []string{"DIFFERENT_ARG"}
	p.BootPrompt = "DIFFERENT_PROMPT"
	second, err := rig.svc.teamPlanEvidence(p, "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatal("secret-bearing values influenced digest")
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "SYNTHETIC") || first.Placement != "hub_asserted_not_worker_verified" {
		t.Fatal("unsafe projection")
	}
	stored, err := rig.svc.Store.GetLaunchPlan(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ActivationEvidence.Fingerprint != before {
		t.Fatal("persisted create evidence changed")
	}
}

func TestTeamActivationSandboxCaptureUsesLaunchTimeProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rig := newCodexRig(t)
	a := rig.svc.Catalog.Agents["agent"]
	a.Permissions.DefaultSandbox = "workspace-only"
	rig.svc.Catalog.Agents["agent"] = a
	rig.svc.Catalog.SandboxProfiles = sandboxProfiles()
	provider := rig.svc.Catalog.Providers["codex-cli"]
	provider.Provider = "claude"
	provider.ID = "claude-pty"
	provider.RuntimeKind = config.RuntimeKindPTY
	rig.svc.Catalog.Providers[provider.ID] = provider
	l := rig.svc.Catalog.Launches["codex-launch"]
	l.Provider = provider.ID
	rig.svc.Catalog.Launches[l.ID] = l
	var profiles []sandbox.Profile
	rig.svc.factories[provider.ID] = func(p *launch.Plan) (agentsessions.Runtime, error) {
		rt, err := stub.New(p)
		return teamProfileRuntime{Runtime: rt, profiles: &profiles}, err
	}
	created, err := rig.svc.CreateTeamSession(t.Context(), "sandbox-evidence", l.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := rig.svc.Store.GetLaunchPlan(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rig.svc.Store.TeamEvidence(t.Context(), "launched", created.SessionID); err == nil {
		t.Fatal("create claimed successful launch")
	}
	profile := rig.svc.Catalog.SandboxProfiles["workspace-only"]
	profile.Net = !profile.Net
	rig.svc.Catalog.SandboxProfiles["workspace-only"] = profile
	if _, err = rig.svc.LaunchSession(created.SessionID); err != nil {
		t.Fatal(err)
	}
	launched, err := rig.svc.Store.TeamEvidence(t.Context(), "launched", created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if launched.SandboxDigest == persisted.ActivationEvidence.SandboxDigest || len(profiles) != 1 || profiles[0].Net != profile.Net {
		t.Fatal("launch evidence did not capture actual changed sandbox")
	}
	if launched.ExecutionBasis != "legacy_catalog" {
		t.Fatal("false definition execution claim")
	}
}

func TestTeamActivationInstalledSandboxIsSeparateAndCanonical(t *testing.T) {
	svc, write := newLaunchCatalogService(t)
	write("agents/a.yaml", "id: a\nname: A\npermissions:\n  default_sandbox: fixture\n")
	original := "id: fixture\nfs:\n  read: [/SYNTHETIC_PRIVATE_PATH_A, /SYNTHETIC_PRIVATE_PATH_B]\n"
	write("sandbox-profiles/nonmatching-filename.yaml", original)
	if _, err := svc.ReloadCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := svc.InspectTeamLaunch(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	write("sandbox-profiles/nonmatching-filename.yaml", "id: fixture\nfs:\n  read: [/SYNTHETIC_PRIVATE_PATH_C, /SYNTHETIC_PRIVATE_PATH_B]\n")
	installed, err := svc.InspectTeamLaunch(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	if before.InstalledSandboxFileDigest == installed.InstalledSandboxFileDigest || before.EffectiveSandboxPolicyDigest != installed.EffectiveSandboxPolicyDigest || installed.InstalledSandboxPolicyDigest == installed.EffectiveSandboxPolicyDigest {
		t.Fatal("installed file was confused with active policy")
	}
	if _, err = svc.ReloadCatalog(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := svc.InspectTeamLaunch(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	if after.EffectiveSandboxPolicyDigest == before.EffectiveSandboxPolicyDigest || after.CatalogGeneration <= before.CatalogGeneration {
		t.Fatal("reload did not update effective observation")
	}
	p, err := svc.BuildLaunchPlan(CreateSessionInput{LaunchID: "first"})
	if err != nil {
		t.Fatal(err)
	}
	one := svc.Catalog.SandboxProfiles["fixture"]
	two := one
	two.FS.Read = []string{one.FS.Read[1], one.FS.Read[0]}
	first, err := svc.teamPlanEvidence(p, "", &one)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.teamPlanEvidence(p, "", &two)
	if err != nil {
		t.Fatal(err)
	}
	if first.EffectiveSandboxPolicyDigest != second.EffectiveSandboxPolicyDigest || first.Fingerprint != second.Fingerprint {
		t.Fatal("selector order changed canonical evidence")
	}
	public := teamactivation.Evidence{Launch: after, Created: &first, Launched: &second, EvidenceDigest: "PRIVATE_AGGREGATE"}.General()
	raw, _ := json.Marshal(public)
	if strings.Contains(string(raw), "SYNTHETIC_PRIVATE_PATH") || strings.Contains(string(raw), after.EffectiveSandboxPolicyDigest) || strings.Contains(string(raw), after.InstalledSandboxFileDigest) || strings.Contains(string(raw), "PRIVATE_AGGREGATE") {
		t.Fatal("public projection leaked private selectors or aggregate")
	}
}

func TestTeamActivationEnforcementOutcomeIsSeparateFromPolicy(t *testing.T) {
	rig := newCodexRig(t)
	rig.svc.recordTeamSandboxOutcome(t.Context(), "downgraded", agentsessions.SandboxOutcome{Enforced: true, Unsupported: []string{"unsupported-feature"}, Diagnostics: []string{"SYNTHETIC_PRIVATE_DIAGNOSTIC"}})
	out, err := rig.svc.Store.TeamEvidence(t.Context(), "sandbox_result", "downgraded")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if out.SandboxEnforcement != "downgraded" || strings.Contains(string(raw), "SYNTHETIC_PRIVATE_DIAGNOSTIC") {
		t.Fatal("outcome inferred or diagnostics leaked")
	}
	rig.svc.recordTeamSandboxOutcome(t.Context(), "enforced", agentsessions.SandboxOutcome{Enforced: true})
	out, err = rig.svc.Store.TeamEvidence(t.Context(), "sandbox_result", "enforced")
	if err != nil || out.SandboxEnforcement != "enforced" {
		t.Fatal("enforcement outcome lost")
	}
}
