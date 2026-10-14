package app

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestWithEditableAgentsAppendsOneFlagPerID(t *testing.T) {
	base := []string{"mcp", "--proxy"}
	got := withEditableAgents(slices.Clone(base), []string{"chat", "zz-probe"})
	want := []string{"mcp", "--proxy", "--editable-agent", "chat", "--editable-agent", "zz-probe"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	if got := withEditableAgents(slices.Clone(base), nil); !slices.Equal(got, base) {
		t.Fatalf("no allowlist changed the argv: %v", got)
	}
}

func TestCatalogEditableAgentsReadsCatalogAgentOnly(t *testing.T) {
	svc := &Service{Catalog: &config.Catalog{Agents: map[string]config.Agent{
		"aom":  {ID: "aom", EditableAgents: []string{"chat"}},
		"none": {ID: "none"},
	}}}
	if got := svc.catalogEditableAgents(&launch.Plan{LogicalAgentID: "aom"}); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("aom = %v", got)
	}
	if got := svc.catalogEditableAgents(&launch.Plan{LogicalAgentID: "none"}); len(got) != 0 {
		t.Fatalf("an agent with no editable_agents got %v", got)
	}
	member := &launch.Plan{LogicalAgentID: "aom#1", CatalogAgentID: "aom", TeamMember: true}
	if got := svc.catalogEditableAgents(member); len(got) != 0 {
		t.Fatalf("team member took the base agent's allowlist: %v", got)
	}
}

func TestApplyCatalogIdentityCarriesEditableAgents(t *testing.T) {
	svc := &Service{Catalog: &config.Catalog{Agents: map[string]config.Agent{"aom": {ID: "aom", EditableAgents: []string{"chat"}}}}}
	plan := &launch.Plan{LogicalAgentID: "aom"}
	svc.applyCatalogIdentity(plan)
	if !slices.Equal(plan.EditableAgents, []string{"chat"}) {
		t.Fatalf("plan.EditableAgents = %v", plan.EditableAgents)
	}
}

func TestWorkerLaunchSendsEditableAgentsOnlyToWorkersThatPlantThem(t *testing.T) {
	newPlan := func() *launch.Plan {
		return &launch.Plan{LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderBrand: "claude", EditableAgents: []string{"chat"}}
	}
	t.Run("capable worker", func(t *testing.T) {
		r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred, workerwire.FeatureEditableAgents)
		plan := newPlan()
		row := store.SessionRow{ID: "ok", WorkerID: r.worker, LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: t.TempDir(), State: "created"}
		if err := r.db.CreateSession(row, plan); err != nil {
			t.Fatal(err)
		}
		if _, err := r.svc.launchWorkerSession(context.Background(), &row, plan); err != nil {
			t.Fatal(err)
		}
		spec := <-r.specs
		if !slices.Equal(spec.EditableAgents, []string{"chat"}) {
			t.Fatalf("spec.EditableAgents = %v", spec.EditableAgents)
		}
	})
	t.Run("old worker fails closed", func(t *testing.T) {
		r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
		plan := newPlan()
		row := store.SessionRow{ID: "old", WorkerID: r.worker, LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: t.TempDir(), State: "created"}
		if err := r.db.CreateSession(row, plan); err != nil {
			t.Fatal(err)
		}
		_, err := r.svc.launchWorkerSession(context.Background(), &row, plan)
		if err == nil || !strings.Contains(err.Error(), "editable_agents") {
			t.Fatalf("err = %v, want refusal rather than an unrestricted session", err)
		}
		if got, _ := r.db.GetSession("old"); got.State != "failed" {
			t.Fatalf("state = %s", got.State)
		}
	})
}
