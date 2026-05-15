package app

import (
	"context"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestCompileSharedLaunchStoresProvenance(t *testing.T) {
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog: &config.Catalog{
			Global: config.Global{Version: "test"},
		},
	}
	plan := &launch.Plan{
		LaunchID:       "demo",
		ProjectID:      "project",
		LogicalAgentID: "agent",
		ProviderID:     "claude-pty",
		ProviderBrand:  "claude",
		RuntimeKind:    config.RuntimeKindPTY,
		RepoRoot:       t.TempDir(),
		WriteHome:      t.TempDir(),
		WorkspaceMode:  "worktree",
		Command:        "claude",
		BootPrompt:     "boot",
		BootMode:       "stdin",
	}

	if err := svc.compileSharedLaunch(context.Background(), plan); err != nil {
		t.Fatalf("compileSharedLaunch: %v", err)
	}
	if plan.Shared == nil {
		t.Fatal("Shared is nil")
	}
	if plan.Shared.PlanHash == "" {
		t.Fatal("Shared.PlanHash empty")
	}
	if plan.Shared.ProviderID != "claude" {
		t.Fatalf("Shared.ProviderID = %q, want claude", plan.Shared.ProviderID)
	}
	if plan.Shared.RuntimeKind != "pty" {
		t.Fatalf("Shared.RuntimeKind = %q, want pty", plan.Shared.RuntimeKind)
	}
}

func TestAgentLaunchPlanCarriesInjection(t *testing.T) {
	svc := &Service{}
	plan := &launch.Plan{
		LaunchID:       "demo",
		ProjectID:      "project",
		LogicalAgentID: "agent",
		ProviderID:     "claude-pty",
		ProviderBrand:  "claude",
		RuntimeKind:    config.RuntimeKindPTY,
		RepoRoot:       t.TempDir(),
		WorkspaceMode:  "worktree",
		Command:        "claude",
		NativeFiles: []launch.NativeFile{
			{Kind: "raw", RelPath: ".mux/context.md", Content: "context\n", Mode: 0o600},
		},
		BootDirOverlay: map[string]string{"extra.md": "overlay\n"},
	}

	lp := svc.agentLaunchPlan(plan, t.TempDir())
	if len(lp.Injection.NativeFiles) != 1 {
		t.Fatalf("NativeFiles len = %d, want 1", len(lp.Injection.NativeFiles))
	}
	if lp.Injection.NativeFiles[0].RelPath != ".mux/context.md" {
		t.Fatalf("NativeFiles[0] = %#v", lp.Injection.NativeFiles[0])
	}
	if lp.Injection.BootDirOverlay["extra.md"] != "overlay\n" {
		t.Fatalf("BootDirOverlay = %#v", lp.Injection.BootDirOverlay)
	}
}
