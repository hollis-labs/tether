package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/agentkit/agentlaunch"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// specWiringPaths resolves the in-repo fixtures the Spec-path wiring tests
// run against: the specresolve fixture catalog (runtime-binding + agent
// records covering every runner/agent the corpus references) and the
// shared S5 LaunchSpec corpus at the repo root.
func specWiringPaths(t *testing.T) (catalogRoot, specsRoot string) {
	t.Helper()
	catalogRoot, err := filepath.Abs(filepath.Join("..", "specresolve", "testdata", "catalog"))
	if err != nil {
		t.Fatalf("resolve fixture catalog root: %v", err)
	}
	specsRoot, err = filepath.Abs(filepath.Join("..", "..", "testdata", "launch-specs"))
	if err != nil {
		t.Fatalf("resolve corpus root: %v", err)
	}
	return catalogRoot, specsRoot
}

// specWiringService builds a Service pointed at the Spec-path fixtures.
// The catalog the registry/resolver ingest is the specresolve fixture
// catalog; Catalog here only needs Global for the version stamp.
func specWiringService(t *testing.T) *Service {
	t.Helper()
	catalogRoot, _ := specWiringPaths(t)
	return &Service{
		CatalogRoot: catalogRoot,
		Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
	}
}

// TestAgentLaunchPlanForSpecEngine proves that with the toggle set to
// "spec" and the specs root pointed at testdata/launch-specs/, a launch id
// resolves through the Spec path to a Validate()-clean
// agentlaunch.LaunchPlan.
func TestAgentLaunchPlanForSpecEngine(t *testing.T) {
	t.Setenv(EnvLaunchEngine, "spec")
	_, specsRoot := specWiringPaths(t)
	t.Setenv(EnvLaunchSpecsRoot, specsRoot)

	svc := specWiringService(t)
	if !svc.LaunchEngineIsSpec() {
		t.Fatal("LaunchEngineIsSpec = false, want true with TETHER_LAUNCH_ENGINE=spec")
	}

	// launch.Plan.LaunchID is the only field the Spec path reads — it is
	// the daemon's bookkeeping currency and carries the launch id.
	plan := &launch.Plan{LaunchID: "tether-claude"}
	workspaceDir := t.TempDir()

	lp, err := svc.agentLaunchPlanFor(context.Background(), plan, workspaceDir)
	if err != nil {
		t.Fatalf("agentLaunchPlanFor(spec): %v", err)
	}
	if err := lp.Validate(); err != nil {
		t.Fatalf("Spec-resolved LaunchPlan failed Validate(): %v", err)
	}
	// The daemon owns the workspace dir; the wiring must fold it on.
	if lp.Workspace.WorkspaceDir != workspaceDir {
		t.Fatalf("Workspace.WorkspaceDir = %q, want %q", lp.Workspace.WorkspaceDir, workspaceDir)
	}
	// Sanity: the Spec path resolved the corpus bag, not an empty plan.
	if lp.Project.ID == "" {
		t.Fatal("Spec-resolved LaunchPlan has empty Project.ID")
	}
	// Interactive front-end stamps an interactive launch mode.
	if lp.Mode != agentlaunch.LaunchInteractive {
		t.Fatalf("Mode = %v, want LaunchInteractive", lp.Mode)
	}
}

// TestAgentLaunchPlanForCatalogEngineDefault asserts that with the toggle
// OFF (default — no env var, no config field), agentLaunchPlanFor takes
// the legacy catalog path: it builds the plan from the launch.Plan via
// agentLaunchPlan and never touches the Spec resolver. Pointing the specs
// root at a nonexistent directory proves the Spec path was not taken — if
// it were, resolution would fail.
func TestAgentLaunchPlanForCatalogEngineDefault(t *testing.T) {
	t.Setenv(EnvLaunchEngine, "")
	t.Setenv(EnvLaunchSpecsRoot, filepath.Join(t.TempDir(), "does-not-exist"))

	svc := specWiringService(t)
	if svc.LaunchEngineIsSpec() {
		t.Fatal("LaunchEngineIsSpec = true with no toggle set, want false (default catalog)")
	}

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
		BootPrompt:     "boot",
		BootMode:       "stdin",
	}
	workspaceDir := t.TempDir()

	got, err := svc.agentLaunchPlanFor(context.Background(), plan, workspaceDir)
	if err != nil {
		t.Fatalf("agentLaunchPlanFor(catalog): %v", err)
	}
	// The catalog path must be byte-identical to a direct agentLaunchPlan
	// call — the toggle-off invariant.
	want := svc.agentLaunchPlan(plan, workspaceDir)
	if got.Project.ID != want.Project.ID || got.Provider.ID != want.Provider.ID ||
		got.Agent.ID != want.Agent.ID || got.Mode != want.Mode {
		t.Fatalf("catalog-path plan diverged from agentLaunchPlan:\n got=%+v\nwant=%+v", got, want)
	}
	if got.Project.ID != "project" {
		t.Fatalf("Project.ID = %q, want catalog-derived %q", got.Project.ID, "project")
	}
}

// TestLaunchEngineConfigDefaultDrivesSpec proves the config-backed default
// (global.yaml catalog.defaults.launch_engine) selects the Spec engine
// when the env var is unset.
func TestLaunchEngineConfigDefaultDrivesSpec(t *testing.T) {
	t.Setenv(EnvLaunchEngine, "")
	_, specsRoot := specWiringPaths(t)
	t.Setenv(EnvLaunchSpecsRoot, specsRoot)

	catalogRoot, _ := specWiringPaths(t)
	cat := &config.Catalog{Global: config.Global{Version: "test"}}
	cat.Global.Catalog.Defaults.LaunchEngine = "spec"
	svc := &Service{CatalogRoot: catalogRoot, Catalog: cat}

	if !svc.LaunchEngineIsSpec() {
		t.Fatal("LaunchEngineIsSpec = false, want true from config default")
	}
	lp, err := svc.agentLaunchPlanFor(context.Background(), &launch.Plan{LaunchID: "tether-claude"}, t.TempDir())
	if err != nil {
		t.Fatalf("agentLaunchPlanFor(spec via config): %v", err)
	}
	if err := lp.Validate(); err != nil {
		t.Fatalf("config-spec LaunchPlan failed Validate(): %v", err)
	}
}
