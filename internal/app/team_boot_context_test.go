package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/harness/agentlaunch/launcher"
	providerplant "github.com/hollis-labs/substrate/harness/agentlaunch/planting"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchartifacts"
	"github.com/hollis-labs/tether/internal/launchartifacts/testfixture"
	"github.com/hollis-labs/tether/internal/store"
)

func TestTeamBootContextSurvivesBothLaunchEngines(t *testing.T) {
	for _, engine := range []string{"catalog", "spec"} {
		t.Run(engine, func(t *testing.T) {
			t.Setenv(EnvLaunchEngine, engine)
			_, specs := specWiringPaths(t)
			t.Setenv(EnvLaunchSpecsRoot, specs)
			svc := specWiringService(t)
			plan := &launch.Plan{LaunchID: "tether-claude", TeamMember: true, ProviderID: "claude-pty", ProviderBrand: "claude", Command: "claude", RuntimeKind: config.RuntimeKindPTY, RepoRoot: t.TempDir(), LogicalAgentID: "agent", ProjectID: "project", BootPrompt: "catalog boot", BootMode: "planted"}
			context := TeamBootContext{RunID: "run", Slot: "worker", Mission: "Implement the accepted task", Brief: "Preserve the retained context"}
			if err := applyTeamBootContext(plan, context); err != nil {
				t.Fatal(err)
			}
			workspace := t.TempDir()
			baseline, err := svc.engineLaunchPlan(contextBackground(), plan, workspace)
			if err != nil {
				t.Fatal(err)
			}
			svc.applyClaudeStrictMCP(&baseline)
			got, err := svc.agentLaunchPlanFor(contextBackground(), plan, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if got.Injection.BootDirOverlay["MISSION.md"] != context.Mission || got.Injection.BootDirOverlay["brief.md"] != context.Brief || !strings.Contains(got.BootProfile.Inline.BootPrompt, teamBootInstructions) {
				t.Fatal("accepted context missing", got.Injection)
			}
			if !reflect.DeepEqual(got.Provider, baseline.Provider) || !reflect.DeepEqual(got.Workspace, baseline.Workspace) || !reflect.DeepEqual(got.MCP, baseline.MCP) || !reflect.DeepEqual(got.Agent, baseline.Agent) {
				t.Fatal("context changed engine authority or roots")
			}
			got.Workspace.TempPrefix = t.TempDir()
			compiled, err := launcher.Compile(contextBackground(), got)
			if err != nil {
				t.Fatal(err)
			}
			prepared, custody, err := launchartifacts.Prepare(contextBackground(), compiled, testfixture.Admission(t, func() any { return compiled }))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = custody.Close() })
			if err = providerplant.Plant(contextBackground(), prepared, providerplant.WithArtifactAuthorization(custody.Authorize)); err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"MISSION.md": context.Mission, "brief.md": context.Brief} {
				content, readErr := os.ReadFile(filepath.Join(prepared.PlantedBootDir, name)) //nolint:gosec // Exact test-owned admitted boot root.
				if readErr != nil || string(content) != want {
					t.Fatal(name, string(content), readErr)
				}
			}
			// Unrelated persisted overlays cannot replace the spec's resolution.
			if engine == "spec" {
				plan.BootDirOverlay["unrelated.md"] = "caller overlay"
				got, err = svc.agentLaunchPlanFor(contextBackground(), plan, workspace)
				if err != nil || got.Injection.BootDirOverlay["unrelated.md"] != "" {
					t.Fatal("general overlay merged", err)
				}
			}
			plan.BootDirOverlay["MISSION.md"] = "changed after seal"
			if _, err = svc.agentLaunchPlanFor(contextBackground(), plan, workspace); !errors.Is(err, teams.ErrConflict) {
				t.Fatal("changed context accepted", err)
			}
		})
	}
}

func contextBackground() context.Context { return context.Background() }

func TestTeamBootContextCreateReplayKeepsOriginalPlan(t *testing.T) {
	rig := newCodexRig(t)
	ctx := TeamBootContext{RunID: "run", Slot: "worker", Mission: "Mission", Brief: "Brief"}
	first, err := rig.svc.CreateTeamSessionWithContext(context.Background(), "team:context", "codex-launch", ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rig.svc.CreateTeamSessionWithContext(context.Background(), "team:context", "codex-launch", ctx)
	if err != nil || !second.Replayed || second.SessionID != first.SessionID {
		t.Fatal(second, err)
	}
	ctx.Brief = "different intent"
	if _, err = rig.svc.CreateTeamSessionWithContext(context.Background(), "team:context", "codex-launch", ctx); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatal("changed context replayed", err)
	}
	plan, err := rig.svc.Store.GetLaunchPlan(first.SessionID)
	if err != nil || plan.BootDirOverlay["brief.md"] != "Brief" || plan.BootDirOverlay["MISSION.md"] != "Mission" || !plan.TeamMember {
		t.Fatal(plan, err)
	}
	if countSessions(t, rig.svc) != 1 {
		t.Fatal("retry created a replacement")
	}
}

func TestTeamBootContextRejectsCollisionsAndInvalidContext(t *testing.T) {
	ctx := TeamBootContext{RunID: "run", Slot: "worker", Mission: "Mission"}
	for _, plan := range []*launch.Plan{
		{BootDirOverlay: map[string]string{"MISSION.md": "catalog-owned"}},
		{NativeFiles: []launch.NativeFile{{RelPath: "brief.md", Content: "catalog-owned"}}},
	} {
		if err := applyTeamBootContext(plan, ctx); !errors.Is(err, teams.ErrConflict) {
			t.Fatal(err)
		}
	}
	for _, ctx := range []TeamBootContext{{Mission: "missing source"}, {RunID: "run", Slot: "slot", Mission: "bad\x00text"}, {RunID: "run", Slot: "slot", Brief: strings.Repeat("x", 64*1024+1)}} {
		if err := applyTeamBootContext(&launch.Plan{}, ctx); err == nil {
			t.Fatal("invalid context accepted")
		}
	}
	if teamCreateDigest("launch", TeamBootContext{}) != requestDigest(struct{ Op, Launch string }{"team-create", "launch"}) {
		t.Fatal("legacy create digest changed")
	}
}
