package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/launch"
)

// buildTestService is the minimal Service for agent-ops unit tests. It
// populates only the fields applyAgentOps reads — Catalog (Agents) and
// CatalogRoot for skill discovery.
func buildTestService(t *testing.T, agents map[string]config.Agent, catalogRoot string) *Service {
	t.Helper()
	return &Service{
		Catalog: &config.Catalog{
			Agents:   agents,
			Projects: map[string]config.Project{},
		},
		CatalogRoot: catalogRoot,
	}
}

func basePlan() *launch.Plan {
	return &launch.Plan{
		LaunchID:       "test-launch",
		ProjectID:      "test-project",
		LogicalAgentID: "test-agent",
		ProviderID:     "claude-code",
		BootPrompt:     "base boot fragments\n",
		Env:            map[string]string{"BASE_KEY": "base"},
	}
}

func TestApplyAgentOps_CatalogOnly_LeavesPlanIntact(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", Name: "Test"},
	}, t.TempDir())
	plan := basePlan()
	if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch"}); err != nil {
		t.Fatalf("applyAgentOps: %v", err)
	}
	if plan.BootPrompt != "base boot fragments\n" {
		t.Errorf("BootPrompt = %q; want unchanged base", plan.BootPrompt)
	}
	if plan.Env["BASE_KEY"] != "base" {
		t.Errorf("Env BASE_KEY lost: %v", plan.Env)
	}
}

func TestApplyAgentOps_SystemPromptAppended(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {
			ID:           "test-agent",
			SystemPrompt: "You are a careful refactorer.",
		},
	}, t.TempDir())
	plan := basePlan()
	if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.BootPrompt, "base boot fragments") {
		t.Errorf("base prompt lost: %q", plan.BootPrompt)
	}
	if !strings.Contains(plan.BootPrompt, "You are a careful refactorer.") {
		t.Errorf("SystemPrompt not appended: %q", plan.BootPrompt)
	}
	if !strings.Contains(plan.BootPrompt, "# System") {
		t.Errorf("missing # System heading: %q", plan.BootPrompt)
	}
}

func TestApplyAgentOps_OverrideSystemPromptWins(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {
			ID:           "test-agent",
			SystemPrompt: "agent-level prompt",
		},
	}, t.TempDir())
	plan := basePlan()
	in := CreateSessionInput{
		LaunchID: "test-launch",
		Override: `{"system_prompt": "explicit override prompt", "env": {"OVERRIDE_KEY": "yes"}}`,
	}
	if err := svc.applyAgentOps(plan, in); err != nil {
		t.Fatal(err)
	}
	if plan.BootPrompt != "explicit override prompt" {
		t.Errorf("BootPrompt = %q; want %q", plan.BootPrompt, "explicit override prompt")
	}
	if plan.Env["OVERRIDE_KEY"] != "yes" {
		t.Errorf("override env not applied: %v", plan.Env)
	}
	if plan.Env["BASE_KEY"] != "base" {
		t.Errorf("base env clobbered: %v", plan.Env)
	}
}

func TestApplyAgentOps_BootPromptOverrideWinsLast(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", SystemPrompt: "agent-level"},
	}, t.TempDir())
	plan := basePlan()
	in := CreateSessionInput{
		LaunchID:           "test-launch",
		BootPromptOverride: "raw boot prompt",
		Override:           `{"system_prompt": "soft override"}`,
	}
	if err := svc.applyAgentOps(plan, in); err != nil {
		t.Fatal(err)
	}
	if plan.BootPrompt != "raw boot prompt" {
		t.Errorf("BootPrompt = %q; want raw boot prompt (BootPromptOverride wins last)", plan.BootPrompt)
	}
}

func TestApplyAgentOps_AgentFile_FieldMerged(t *testing.T) {
	tmp := t.TempDir()
	agentFile := filepath.Join(tmp, "tier2.yaml")
	body := `id: tier2-agent
name: Tier 2 Agent
system_prompt: "Tier 2 system prompt"
agent_prompt: "I am Tier 2"
permissions:
  network: true
`
	if err := os.WriteFile(agentFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", Name: "Catalog", Permissions: config.AgentPermissions{DefaultSandbox: "workspace-only"}},
	}, tmp)
	plan := basePlan()
	in := CreateSessionInput{LaunchID: "test-launch", AgentFile: agentFile}
	if err := svc.applyAgentOps(plan, in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.BootPrompt, "Tier 2 system prompt") {
		t.Errorf("AgentFile SystemPrompt not merged: %q", plan.BootPrompt)
	}
	if !strings.Contains(plan.BootPrompt, "I am Tier 2") {
		t.Errorf("AgentFile AgentPrompt not merged: %q", plan.BootPrompt)
	}
}

func TestApplyAgentOps_AgentInline_OverridesFile(t *testing.T) {
	tmp := t.TempDir()
	agentFile := filepath.Join(tmp, "file.yaml")
	if err := os.WriteFile(agentFile, []byte("id: f\nsystem_prompt: from file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", SystemPrompt: "from catalog"},
	}, tmp)
	plan := basePlan()
	in := CreateSessionInput{
		LaunchID:    "test-launch",
		AgentFile:   agentFile,
		AgentInline: `{"id":"i","system_prompt":"from inline"}`,
	}
	if err := svc.applyAgentOps(plan, in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.BootPrompt, "from inline") {
		t.Errorf("AgentInline did not win: %q", plan.BootPrompt)
	}
	if strings.Contains(plan.BootPrompt, "from file") || strings.Contains(plan.BootPrompt, "from catalog") {
		t.Errorf("lower-precedence content leaked: %q", plan.BootPrompt)
	}
}

func TestApplyAgentOps_BootProfile_PopulatesMCPServers(t *testing.T) {
	tmp := t.TempDir()
	profilePath := filepath.Join(tmp, "profile.yaml")
	body := `id: test-profile
display_name: Test
mcp_servers: [vanta, clockwork, cerberus]
`
	if err := os.WriteFile(profilePath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent"},
	}, tmp)
	plan := basePlan()
	in := CreateSessionInput{LaunchID: "test-launch", BootProfileFile: profilePath}
	if err := svc.applyAgentOps(plan, in); err != nil {
		t.Fatal(err)
	}
	got := plan.Env["MUX_MCP_SERVERS"]
	if got != "vanta,clockwork,cerberus" {
		t.Errorf("MUX_MCP_SERVERS = %q; want vanta,clockwork,cerberus", got)
	}
}

func TestApplyAgentOps_ProviderOverrides_AppliesEnvAndArgs(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {
			ID: "test-agent",
			ProviderOverrides: map[string]config.ProviderOverride{
				"claude-code": {
					ExtraArgs: []string{"--extra"},
					Env:       map[string]string{"CLAUDE_FLAG": "1"},
				},
				"codex-app-server": {
					ExtraArgs: []string{"--codex-only"},
				},
			},
		},
	}, t.TempDir())
	plan := basePlan() // ProviderID: claude-code
	plan.Args = []string{"--existing"}
	if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch"}); err != nil {
		t.Fatal(err)
	}
	if plan.Env["CLAUDE_FLAG"] != "1" {
		t.Errorf("provider-override env not applied: %v", plan.Env)
	}
	wantArgs := []string{"--existing", "--extra"}
	if len(plan.Args) != len(wantArgs) {
		t.Fatalf("args = %v; want %v", plan.Args, wantArgs)
	}
	for i, w := range wantArgs {
		if plan.Args[i] != w {
			t.Errorf("Args[%d] = %q; want %q", i, plan.Args[i], w)
		}
	}
}

func TestApplyAgentOps_BadAgentFile_HardErrors(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent"},
	}, t.TempDir())
	plan := basePlan()
	in := CreateSessionInput{
		LaunchID:  "test-launch",
		AgentFile: filepath.Join(t.TempDir(), "does-not-exist.yaml"),
	}
	if err := svc.applyAgentOps(plan, in); err == nil {
		t.Fatal("expected error for missing agent file, got nil")
	}
}

func TestApplyAgentOps_BadOverrideJSON_HardErrors(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent"},
	}, t.TempDir())
	plan := basePlan()
	in := CreateSessionInput{LaunchID: "test-launch", Override: `{not json`}
	if err := svc.applyAgentOps(plan, in); err == nil {
		t.Fatal("expected error for malformed override JSON, got nil")
	}
}

func TestApplyAgentOps_SkillsCompiled(t *testing.T) {
	tmp := t.TempDir()
	// Plant a system-layer skill discoverable via DefaultLayers(catalogRoot, catalogRoot).
	skillsDir := filepath.Join(tmp, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillBody := "---\nid: refactor\nname: Refactor\n---\nrefactor body\n"
	if err := os.WriteFile(filepath.Join(skillsDir, "refactor.md"), []byte(skillBody), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", Skills: []string{"refactor"}},
	}, tmp)
	plan := basePlan() // provider claude-code
	if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch"}); err != nil {
		t.Fatalf("applyAgentOps: %v", err)
	}
	if !strings.Contains(plan.BootPrompt, "refactor body") {
		t.Errorf("compiled skill body missing from BootPrompt: %q", plan.BootPrompt)
	}
	// Claude compiler emits .claude/skills/<id>.md; the relpath comment should show.
	if !strings.Contains(plan.BootPrompt, ".claude/skills/refactor.md") {
		t.Errorf("compiled skill marker missing: %q", plan.BootPrompt)
	}
}

func TestApplyAgentOps_MissingSkillHardErrors(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", Skills: []string{"no-such-skill"}},
	}, t.TempDir())
	plan := basePlan()
	if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch"}); err == nil {
		t.Fatal("expected error for missing skill, got nil")
	}
}
