package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
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

func TestApplyAgentOps_BootPromptAppend(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent"},
	}, t.TempDir())
	plan := basePlan()
	in := CreateSessionInput{
		LaunchID:         "test-launch",
		BootPromptAppend: "read tasks/README.md",
	}
	if err := svc.applyAgentOps(plan, in); err != nil {
		t.Fatalf("applyAgentOps: %v", err)
	}
	if !strings.Contains(plan.BootPrompt, "base boot fragments") {
		t.Fatalf("base prompt lost: %q", plan.BootPrompt)
	}
	if !strings.Contains(plan.BootPrompt, "read tasks/README.md") {
		t.Fatalf("append missing: %q", plan.BootPrompt)
	}
	if plan.BootPromptAppend != "read tasks/README.md" {
		t.Fatalf("BootPromptAppend = %q", plan.BootPromptAppend)
	}
}

func TestRefreshBootProfilePrompt_PreservesAppend(t *testing.T) {
	tmp := t.TempDir()
	profileFile := filepath.Join(tmp, "boot.yaml")
	body := `id: torque-worker
identity:
  lineage_alias: torque.worker
  profile_id: torque-worker
  role: worker
  project: tether
  work_root: /before
slots: {}
`
	if err := os.WriteFile(profileFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent"},
	}, tmp)
	plan := basePlan()
	plan.BootProfileFile = profileFile
	plan.WorkRoot = "/materialized/worktree"
	plan.BootPromptAppend = "read tasks/README.md"
	plan.BootPrompt = "stale prompt\n\nread tasks/README.md\n"

	if err := svc.RefreshBootProfilePrompt(context.Background(), plan); err != nil {
		t.Fatalf("RefreshBootProfilePrompt: %v", err)
	}
	if !strings.Contains(plan.BootPrompt, "work_root:       /materialized/worktree") {
		t.Fatalf("refreshed prompt did not use materialized work root: %q", plan.BootPrompt)
	}
	if strings.Count(plan.BootPrompt, "read tasks/README.md") != 1 {
		t.Fatalf("append should be preserved once, got prompt: %q", plan.BootPrompt)
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

// TestApplyAgentOps_PermissionMode_CallerOverride pins that a caller-provided
// agent (AgentInline) overriding permissions.permission_mode is honored:
// mergeAgent carries the field, and applyPermissionMode reconciles both
// plan.PermissionMode and the --dangerously-skip-permissions flag that
// launch.Resolve baked in from the catalog agent. --mcp-config is left alone.
func TestApplyAgentOps_PermissionMode_CallerOverride(t *testing.T) {
	// Simulate the plan as launch.Resolve leaves it for a claude provider.
	claudePlan := func(mode string, args ...string) *launch.Plan {
		p := basePlan()
		p.ProviderBrand = "claude"
		p.PermissionMode = mode
		p.Args = append([]string(nil), args...)
		return p
	}

	t.Run("inline default beats catalog bypass — skip flag removed", func(t *testing.T) {
		svc := buildTestService(t, map[string]config.Agent{
			"test-agent": {ID: "test-agent", Permissions: config.AgentPermissions{PermissionMode: config.PermissionModeBypass}},
		}, t.TempDir())
		plan := claudePlan(config.PermissionModeBypass, "--mcp-config", ".mcp.json", "--dangerously-skip-permissions")
		in := CreateSessionInput{LaunchID: "test-launch", AgentInline: `{"id":"i","permissions":{"permission_mode":"default"}}`}
		if err := svc.applyAgentOps(plan, in); err != nil {
			t.Fatal(err)
		}
		if plan.PermissionMode != config.PermissionModeDefault {
			t.Errorf("PermissionMode = %q, want default", plan.PermissionMode)
		}
		if slices.Contains(plan.Args, "--dangerously-skip-permissions") {
			t.Errorf("--dangerously-skip-permissions should be removed: %v", plan.Args)
		}
		if !slices.Contains(plan.Args, "--mcp-config") {
			t.Errorf("--mcp-config must be preserved: %v", plan.Args)
		}
	})

	t.Run("inline bypass beats catalog default — skip flag added", func(t *testing.T) {
		svc := buildTestService(t, map[string]config.Agent{
			"test-agent": {ID: "test-agent", Permissions: config.AgentPermissions{PermissionMode: config.PermissionModeDefault}},
		}, t.TempDir())
		plan := claudePlan(config.PermissionModeDefault, "--mcp-config", ".mcp.json")
		in := CreateSessionInput{LaunchID: "test-launch", AgentInline: `{"id":"i","permissions":{"permission_mode":"bypass"}}`}
		if err := svc.applyAgentOps(plan, in); err != nil {
			t.Fatal(err)
		}
		if plan.PermissionMode != config.PermissionModeBypass {
			t.Errorf("PermissionMode = %q, want bypass", plan.PermissionMode)
		}
		if !slices.Contains(plan.Args, "--dangerously-skip-permissions") {
			t.Errorf("--dangerously-skip-permissions should be added: %v", plan.Args)
		}
	})
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

func TestApplyAgentOps_SkillsCompileToNativeFiles(t *testing.T) {
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
	if strings.Contains(plan.BootPrompt, "refactor body") {
		t.Errorf("compiled skill body should not be appended to BootPrompt: %q", plan.BootPrompt)
	}
	if len(plan.NativeFiles) != 1 {
		t.Fatalf("NativeFiles len = %d, want 1", len(plan.NativeFiles))
	}
	if plan.NativeFiles[0].Kind != "skill" {
		t.Errorf("NativeFiles[0].Kind = %q, want skill", plan.NativeFiles[0].Kind)
	}
	if plan.NativeFiles[0].ID != "refactor" {
		t.Errorf("NativeFiles[0].ID = %q, want refactor", plan.NativeFiles[0].ID)
	}
	if !strings.Contains(plan.NativeFiles[0].Content, "refactor body") {
		t.Errorf("compiled skill body missing from native file: %q", plan.NativeFiles[0].Content)
	}
}

func TestApplyAgentOps_PreservesLaunchNativeFiles(t *testing.T) {
	tmp := t.TempDir()
	skillsDir := filepath.Join(tmp, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "refactor.md"), []byte("---\nid: refactor\nname: Refactor\n---\nrefactor body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", Skills: []string{"refactor"}},
	}, tmp)
	plan := basePlan()
	plan.NativeFiles = []launch.NativeFile{
		{Kind: "raw", RelPath: ".mux/context.md", Content: "profile file\n"},
	}
	if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch"}); err != nil {
		t.Fatalf("applyAgentOps: %v", err)
	}
	if len(plan.NativeFiles) != 2 {
		t.Fatalf("NativeFiles len = %d, want profile file + compiled skill", len(plan.NativeFiles))
	}
	if plan.NativeFiles[0].RelPath != ".mux/context.md" {
		t.Fatalf("profile native file was not preserved first: %#v", plan.NativeFiles)
	}
	if plan.NativeFiles[1].Kind != "skill" || plan.NativeFiles[1].ID != "refactor" {
		t.Fatalf("compiled skill missing after profile file: %#v", plan.NativeFiles)
	}
}

// TestApplyAgentOps_CallerInjection_Precedence locks in the documented
// native-file precedence: catalog native files → caller-provided native
// files → compiled agent.skills last.
func TestApplyAgentOps_CallerInjection_Precedence(t *testing.T) {
	tmp := t.TempDir()
	skillsDir := filepath.Join(tmp, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "refactor.md"), []byte("---\nid: refactor\nname: Refactor\n---\nrefactor body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", Skills: []string{"refactor"}},
	}, tmp)
	plan := basePlan()
	// Catalog native file already in the plan (as resolveInjection would leave it).
	plan.NativeFiles = []launch.NativeFile{
		{Kind: "raw", RelPath: ".mux/catalog.md", Content: "catalog file\n"},
	}
	in := CreateSessionInput{
		LaunchID:  "test-launch",
		Injection: `{"native_files":[{"kind":"raw","rel_path":"NOTES.md","content":"caller note\n"}]}`,
	}
	if err := svc.applyAgentOps(plan, in); err != nil {
		t.Fatalf("applyAgentOps: %v", err)
	}
	if len(plan.NativeFiles) != 3 {
		t.Fatalf("NativeFiles len = %d, want catalog + caller + skill", len(plan.NativeFiles))
	}
	if plan.NativeFiles[0].RelPath != ".mux/catalog.md" {
		t.Errorf("NativeFiles[0] = %#v; want catalog file first", plan.NativeFiles[0])
	}
	if plan.NativeFiles[1].RelPath != "NOTES.md" || plan.NativeFiles[1].Content != "caller note\n" {
		t.Errorf("NativeFiles[1] = %#v; want caller-provided file second", plan.NativeFiles[1])
	}
	if plan.NativeFiles[2].Kind != "skill" || plan.NativeFiles[2].ID != "refactor" {
		t.Errorf("NativeFiles[2] = %#v; want compiled skill last", plan.NativeFiles[2])
	}
}

// TestApplyAgentOps_CallerInjection_BootDirOverlayCallerWins asserts the
// caller boot-dir overlay value wins over a catalog overlay entry that shares
// the same rel_path, while non-conflicting entries are preserved.
func TestApplyAgentOps_CallerInjection_BootDirOverlayCallerWins(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent"},
	}, t.TempDir())
	plan := basePlan()
	plan.BootDirOverlay = map[string]string{
		"shared.md":  "catalog value",
		"catalog.md": "catalog only",
	}
	in := CreateSessionInput{
		LaunchID: "test-launch",
		Injection: `{"boot_dir_overlay":[` +
			`{"rel_path":"shared.md","content":"caller value"},` +
			`{"rel_path":"caller.md","content":"caller only"}]}`,
	}
	if err := svc.applyAgentOps(plan, in); err != nil {
		t.Fatalf("applyAgentOps: %v", err)
	}
	if got := plan.BootDirOverlay["shared.md"]; got != "caller value" {
		t.Errorf("shared.md = %q; want caller value (caller wins on duplicate rel_path)", got)
	}
	if got := plan.BootDirOverlay["catalog.md"]; got != "catalog only" {
		t.Errorf("catalog.md = %q; want catalog-only entry preserved", got)
	}
	if got := plan.BootDirOverlay["caller.md"]; got != "caller only" {
		t.Errorf("caller.md = %q; want caller-only entry merged in", got)
	}
}

// TestApplyAgentOps_CallerInjection_SourceResolvesFromCatalogRoot confirms a
// relative `source` path in caller injection resolves against CatalogRoot,
// not the process CWD.
func TestApplyAgentOps_CallerInjection_SourceResolvesFromCatalogRoot(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "fragment.md"), []byte("fragment from catalog root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent"},
	}, tmp)
	plan := basePlan()
	in := CreateSessionInput{
		LaunchID:  "test-launch",
		Injection: `{"native_files":[{"kind":"raw","rel_path":"FRAG.md","source":"fragment.md"}]}`,
	}
	if err := svc.applyAgentOps(plan, in); err != nil {
		t.Fatalf("applyAgentOps: %v", err)
	}
	if len(plan.NativeFiles) != 1 {
		t.Fatalf("NativeFiles len = %d, want 1", len(plan.NativeFiles))
	}
	if plan.NativeFiles[0].Content != "fragment from catalog root\n" {
		t.Errorf("source content = %q; want resolved from CatalogRoot", plan.NativeFiles[0].Content)
	}
}

// TestApplyAgentOps_BadInjectionJSON_HardErrors verifies malformed injection
// JSON aborts plan assembly rather than silently dropping the payload.
func TestApplyAgentOps_BadInjectionJSON_HardErrors(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent"},
	}, t.TempDir())
	plan := basePlan()
	in := CreateSessionInput{LaunchID: "test-launch", Injection: `{not json`}
	if err := svc.applyAgentOps(plan, in); err == nil {
		t.Fatal("expected error for malformed injection JSON, got nil")
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
