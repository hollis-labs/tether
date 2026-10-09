package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/teamimport"
)

// Exercise the imported catalog through the same artifact admission and
// provider planting used by launch. All sources, auth and workspaces are owned
// fakes; no provider process, daemon, helper, registry or user unit is started.
func TestTeamImportPlantsOwnedCodexRoleWithoutCopyingHostConfiguration(t *testing.T) {
	base := t.TempDir()
	boot := filepath.Join(base, "cairn")
	for _, p := range []string{boot, filepath.Join(base, "team"), filepath.Join(base, "scope"), filepath.Join(base, "worktrees"), filepath.Join(boot, ".agents", "skills", "owned", "scripts")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for p, data := range map[string]string{"AGENTS.md": "# Owned role charter\n", ".agents/skills/owned/SKILL.md": "Use scripts/check.sh\n", ".agents/skills/owned/scripts/check.sh": "#!/bin/sh\nexit 0\n", "config.toml": "unimported = true\n", "auth.json": "unimported source credentials"} {
		if err := os.WriteFile(filepath.Join(boot, p), []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(base, "catalog")
	m := teamimport.Manifest{Version: 1, NonSecretSources: true, TeamHome: filepath.Join(base, "team"), TeamSession: "owned", TetherCommand: "tether", WorktreesRoot: filepath.Join(base, "worktrees"), Path: "/owned/tools:/usr/bin", Roles: []teamimport.Role{{Name: "task-owned-1", Role: "task", Project: "owned", Scope: filepath.Join(base, "scope"), BootDir: boot, URN: "msg://agent/agent-mux/agt_owned", Runtime: "codex", Model: "owned-model", Effort: "high", PermissionMode: "bypass", Prompt: "Read the owned assignment.", BaselineEvidence: "fake owned fixture", NativeScopes: []string{"session.write", "message.write", "catalog.write"}, MCPServers: []string{}, MCPTools: []string{}}}}
	if err := teamimport.Write(m, root); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{CatalogRoot: root, Catalog: c}
	plan, err := svc.BuildLaunchPlan(CreateSessionInput{LaunchID: "task-owned-1", BootProfileFile: filepath.Join(root, "boot-profiles", "task-owned-1.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshBootProfilePrompt(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	prepared, err := svc.prepareSharedLaunch(context.Background(), plan, plan.WriteHome, plantContextInput{ArtifactAdmission: testArtifactAdmission(t, plan), TetherCommand: "tether", TetherArgs: []string{"mcp", "--proxy"}, TetherEnv: map[string]string{"TETHER_MCP_SERVERS": "", "TETHER_MCP_TOOLS": "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(root, prepared.PlantedBootDir); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatal("provider planting escaped the owned catalog")
	}
	for _, p := range []string{"AGENTS.md", "config.toml", ".agents/skills/owned/SKILL.md", ".agents/skills/owned/scripts/check.sh"} {
		b, err := os.ReadFile(filepath.Join(prepared.PlantedBootDir, p))
		if err != nil {
			t.Fatalf("planted asset missing %s: %v", p, err)
		}
		if strings.Contains(string(b), "unimported") {
			t.Fatal("source auth/config was copied into the planted provider")
		}
	}
	if !strings.Contains(prepared.Env["TMPDIR"], root) || prepared.Env["TEAM_URN"] != m.Roles[0].URN {
		t.Fatal("actual provider environment lost isolated temp or original identity")
	}
	args := strings.Join(prepared.Argv, " ")
	for _, s := range []string{"owned-model", "model_reasoning_effort", "high", "shell_environment_policy.set", "TEAM_NAME", "task-owned-1"} {
		if !strings.Contains(args, s) {
			t.Fatalf("actual provider arguments lost baseline selection %s", s)
		}
	}
}
