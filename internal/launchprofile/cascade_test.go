package launchprofile_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestResolveSingleProfile(t *testing.T) {
	mem := launchprofile.NewMemorySource()
	mem.AddProfile(&launchprofile.LaunchProfile{
		ID:           "solo",
		Name:         "Solo Agent",
		Provider:     "claude-code",
		Model:        "claude-3-7-sonnet",
		SystemPrompt: "You are a solo agent.",
		Skills:       []string{"git", "go"},
		Env:          map[string]string{"DEBUG": "1"},
	})

	resolver := launchprofile.NewResolver(mem)
	comp, err := resolver.Resolve(context.Background(), launchprofile.CompositionInput{
		Target: "solo",
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if comp.Profile.ID != "solo" {
		t.Errorf("ID = %q, want %q", comp.Profile.ID, "solo")
	}
	if comp.Provider != "claude-code" {
		t.Errorf("Provider = %q, want %q", comp.Provider, "claude-code")
	}
	if len(comp.Skills) != 2 || comp.Skills[0] != "git" || comp.Skills[1] != "go" {
		t.Errorf("Skills = %v, want [git go]", comp.Skills)
	}
	if comp.Env["DEBUG"] != "1" {
		t.Errorf("Env[DEBUG] = %q, want 1", comp.Env["DEBUG"])
	}
	if comp.BootPrompt != "You are a solo agent." {
		t.Errorf("BootPrompt = %q", comp.BootPrompt)
	}
}

func TestResolveExtendsChain(t *testing.T) {
	mem := launchprofile.NewMemorySource()
	mem.AddProfile(&launchprofile.LaunchProfile{
		ID:            "base",
		Name:          "Base Persona",
		Provider:      "codex",
		Model:         "gpt-4o",
		SystemPrompt:  "Base prompt.",
		Skills:        []string{"common", "git"},
		Roles:         []string{"agent"},
		BootFragments: []string{"base-fragment"},
		Env:           map[string]string{"TIER": "base", "SHARED": "root"},
		Body:          "Base guidelines.",
		ProviderOverrides: map[string]launchprofile.ProviderOverride{
			"claude": {
				ExtraArgs: []string{"--verbose"},
				Env:       map[string]string{"MODE": "base"},
			},
		},
	})
	mem.AddProfile(&launchprofile.LaunchProfile{
		ID:            "engineer",
		Extends:       "base",
		Name:          "Engineer Persona",
		Provider:      "claude-code",                  // closest-wins override
		Skills:        []string{"git", "code-review"}, // git deduplicated
		Roles:         []string{"engineer"},
		BootFragments: []string{"eng-fragment"},
		Env:           map[string]string{"TIER": "engineer"}, // overrides TIER
		Body:          "Engineering guidelines.",
		ProviderOverrides: map[string]launchprofile.ProviderOverride{
			"claude": {
				ExtraArgs: []string{"--strict"},
				Env:       map[string]string{"MODE": "engineer"},
			},
		},
	})
	mem.AddProfile(&launchprofile.LaunchProfile{
		ID:            "backend",
		Extends:       "engineer",
		Name:          "Backend Engineer",
		SystemPrompt:  "Backend prompt.", // closest-wins override
		Skills:        []string{"sql", "go"},
		Roles:         []string{"backend"},
		BootFragments: []string{"backend-fragment"},
		Env:           map[string]string{"BACKEND": "true"},
		Body:          "Backend rules.",
	})

	resolver := launchprofile.NewResolver(mem)
	comp, err := resolver.Resolve(context.Background(), launchprofile.CompositionInput{
		Target: "backend",
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	// Closest-wins scalar replacements
	if comp.Profile.Name != "Backend Engineer" {
		t.Errorf("Name = %q, want Backend Engineer", comp.Profile.Name)
	}
	if comp.Provider != "claude-code" {
		t.Errorf("Provider = %q, want claude-code (inherited from engineer)", comp.Provider)
	}
	if comp.Profile.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o (inherited from base)", comp.Profile.Model)
	}
	if comp.SystemPrompt != "Backend prompt." {
		t.Errorf("SystemPrompt = %q, want Backend prompt.", comp.SystemPrompt)
	}

	// Keyed collection union & deduplication
	wantSkills := []string{"common", "git", "code-review", "sql", "go"}
	if len(comp.Skills) != len(wantSkills) {
		t.Fatalf("Skills count = %d, want %d: %v", len(comp.Skills), len(wantSkills), comp.Skills)
	}
	for i, s := range wantSkills {
		if comp.Skills[i] != s {
			t.Errorf("Skills[%d] = %q, want %q", i, comp.Skills[i], s)
		}
	}

	// Roles union
	wantRoles := []string{"agent", "engineer", "backend"}
	if len(comp.Roles) != len(wantRoles) {
		t.Fatalf("Roles = %v, want %v", comp.Roles, wantRoles)
	}

	// Env map merge (descendant wins)
	if comp.Env["TIER"] != "engineer" {
		t.Errorf("Env[TIER] = %q, want engineer", comp.Env["TIER"])
	}
	if comp.Env["SHARED"] != "root" {
		t.Errorf("Env[SHARED] = %q, want root", comp.Env["SHARED"])
	}
	if comp.Env["BACKEND"] != "true" {
		t.Errorf("Env[BACKEND] = %q, want true", comp.Env["BACKEND"])
	}

	// ProviderOverrides nested merge
	claudeOverride := comp.Profile.ProviderOverrides["claude"]
	if claudeOverride.Env["MODE"] != "engineer" {
		t.Errorf("claude override Env[MODE] = %q, want engineer", claudeOverride.Env["MODE"])
	}
	if len(claudeOverride.ExtraArgs) != 2 || claudeOverride.ExtraArgs[0] != "--verbose" || claudeOverride.ExtraArgs[1] != "--strict" {
		t.Errorf("claude override ExtraArgs = %v, want [--verbose, --strict]", claudeOverride.ExtraArgs)
	}

	// Lineage chain
	wantLineage := []string{"base", "engineer", "backend"}
	if len(comp.LineageChain) != len(wantLineage) {
		t.Fatalf("LineageChain = %v, want %v", comp.LineageChain, wantLineage)
	}
	for i, id := range wantLineage {
		if comp.LineageChain[i] != id {
			t.Errorf("LineageChain[%d] = %q, want %q", i, comp.LineageChain[i], id)
		}
	}

	// Body concatenation in root-to-leaf order
	wantBody := "Base guidelines.\n\nEngineering guidelines.\n\nBackend rules."
	if comp.Profile.Body != wantBody {
		t.Errorf("Profile.Body = %q, want %q", comp.Profile.Body, wantBody)
	}
}

func TestResolveExtendsCycle(t *testing.T) {
	mem := launchprofile.NewMemorySource()
	mem.AddProfile(&launchprofile.LaunchProfile{ID: "node-a", Extends: "node-b"})
	mem.AddProfile(&launchprofile.LaunchProfile{ID: "node-b", Extends: "node-c"})
	mem.AddProfile(&launchprofile.LaunchProfile{ID: "node-c", Extends: "node-a"})

	resolver := launchprofile.NewResolver(mem)
	_, err := resolver.Resolve(context.Background(), launchprofile.CompositionInput{
		Target: "node-a",
	})
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	if !errors.Is(err, launchprofile.ErrCycle) {
		t.Errorf("err = %v, want ErrCycle", err)
	}
}

func TestResolveLaunchInputsAndContext(t *testing.T) {
	mem := launchprofile.NewMemorySource()
	mem.AddProfile(&launchprofile.LaunchProfile{
		ID:           "worker",
		Provider:     "claude-stream",
		SystemPrompt: "Worker prompt.",
		Skills:       []string{"base-skill"},
	})
	mem.AddContext(&launchprofile.LaunchContext{
		ID:            "proj-alpha",
		RepoRoot:      "/workspace/alpha",
		TrackingRoot:  "/tracking/alpha",
		KnowledgeBase: []string{"Alpha KB"},
		BootFragments: []string{"Alpha fragments"},
		Workspace: launchprofile.WorkspaceSpec{
			DefaultMode:  "direct",
			WorktreeBase: "/workspace/alpha/.worktrees",
		},
		MCP: launchprofile.MCPConfig{
			Servers: []string{"server-1", "server-2"},
		},
	})

	resolver := launchprofile.NewResolver(mem)
	comp, err := resolver.Resolve(context.Background(), launchprofile.CompositionInput{
		Target:               "worker",
		Scope:                "proj-alpha",
		Provider:             "codex",    // launch-time override
		WorkspaceMode:        "worktree", // launch-time override
		WorktreeName:         "feature-x",
		Skills:               []string{"custom-skill"},
		Prompts:              []string{"custom-prompt"},
		Env:                  map[string]string{"LAUNCH_PARAM": "42"},
		SystemPromptOverride: "Overridden system prompt.",
		PromptAppend:         "Appended note.",
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if comp.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", comp.Provider)
	}
	if comp.WorkspaceMode != "worktree" {
		t.Errorf("WorkspaceMode = %q, want worktree", comp.WorkspaceMode)
	}
	if comp.WorktreeName != "feature-x" {
		t.Errorf("WorktreeName = %q, want feature-x", comp.WorktreeName)
	}
	if comp.SystemPrompt != "Overridden system prompt." {
		t.Errorf("SystemPrompt = %q", comp.SystemPrompt)
	}
	if comp.Env["LAUNCH_PARAM"] != "42" {
		t.Errorf("Env[LAUNCH_PARAM] = %q, want 42", comp.Env["LAUNCH_PARAM"])
	}
	// Context MCP servers injected into MUX_MCP_SERVERS
	if comp.Env["MUX_MCP_SERVERS"] != "server-1,server-2" {
		t.Errorf("Env[MUX_MCP_SERVERS] = %q, want server-1,server-2", comp.Env["MUX_MCP_SERVERS"])
	}
	// Skills union
	if len(comp.Skills) != 2 || comp.Skills[0] != "base-skill" || comp.Skills[1] != "custom-skill" {
		t.Errorf("Skills = %v", comp.Skills)
	}

	// Conversion to Plan
	plan := comp.ToPlan("custom-launch-id")
	if plan.LaunchID != "custom-launch-id" {
		t.Errorf("Plan.LaunchID = %q", plan.LaunchID)
	}
	if plan.ProjectID != "proj-alpha" {
		t.Errorf("Plan.ProjectID = %q, want proj-alpha", plan.ProjectID)
	}
	if plan.RepoRoot != "/workspace/alpha" {
		t.Errorf("Plan.RepoRoot = %q", plan.RepoRoot)
	}
	if plan.Shared == nil || plan.Shared.PlanHash == "" {
		t.Error("Plan.Shared.PlanHash is empty")
	}
}

func TestDeterministicSnapshotAndDigest(t *testing.T) {
	mem := launchprofile.NewMemorySource()
	mem.AddProfile(&launchprofile.LaunchProfile{
		ID:           "deterministic-agent",
		Provider:     "claude-stream",
		SystemPrompt: "Stable prompt.",
		Skills:       []string{"alpha", "beta"},
		Env:          map[string]string{"K1": "V1", "K2": "V2"},
	})

	resolver := launchprofile.NewResolver(mem)
	in := launchprofile.CompositionInput{
		Target: "deterministic-agent",
	}

	snap1, err := resolver.ResolveSnapshot(context.Background(), in)
	if err != nil {
		t.Fatalf("ResolveSnapshot 1 failed: %v", err)
	}
	snap2, err := resolver.ResolveSnapshot(context.Background(), in)
	if err != nil {
		t.Fatalf("ResolveSnapshot 2 failed: %v", err)
	}

	if snap1.Digest == "" {
		t.Fatal("Digest is empty")
	}
	if snap1.Digest != snap2.Digest {
		t.Errorf("Digests differ: %q vs %q", snap1.Digest, snap2.Digest)
	}

	// Mutating an input produces a different digest
	inModified := in
	inModified.Provider = "codex"
	snap3, err := resolver.ResolveSnapshot(context.Background(), inModified)
	if err != nil {
		t.Fatalf("ResolveSnapshot 3 failed: %v", err)
	}
	if snap3.Digest == snap1.Digest {
		t.Errorf("Expected different digest for modified provider, got same: %q", snap3.Digest)
	}
}
