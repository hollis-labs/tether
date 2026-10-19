package app

import (
	"encoding/json"
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
