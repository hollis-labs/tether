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

// CW-20261016-0160: a worker-placed session gets a saved MCP policy at launch, carrying the editable_agents allowlist, so
// the hub's /mcp gateway can serve its agent token and enforce the allowlist; the launch tells the worker so.
func TestWorkerLaunchSavesTheSessionMCPPolicyAndSaysSo(t *testing.T) {
	for name, tc := range map[string]struct {
		editable []string
	}{"with an allowlist": {[]string{"zz-probe", "chat", "chat"}}, "without": {nil}} {
		t.Run(name, func(t *testing.T) {
			r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred, workerwire.FeatureEditableAgents)
			plan := &launch.Plan{LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderBrand: "claude", EditableAgents: tc.editable}
			row := store.SessionRow{ID: "pol", WorkerID: r.worker, LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: t.TempDir(), State: "created"}
			if err := r.db.CreateSession(row, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := r.svc.launchWorkerSession(context.Background(), &row, plan); err != nil {
				t.Fatal(err)
			}
			spec := <-r.specs
			if !spec.MCPPolicy {
				t.Fatal("the launch did not tell the worker the policy was saved")
			}
			policy, err := r.db.SessionMCPPolicy(context.Background(), "pol")
			if err != nil {
				t.Fatalf("no saved MCP policy for the worker session: %v", err)
			}
			want := slices.Compact(slices.Sorted(slices.Values(tc.editable)))
			if policy.SessionID != "pol" || policy.AgentID != "portfolio-manager" || !slices.Equal(policy.EditableAgents, want) || policy.Digest == "" {
				t.Fatalf("saved policy = %+v, want editable %v", policy, want)
			}
		})
	}
}
