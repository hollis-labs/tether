package teamimport_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/bootgen"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/teamimport"
)

func fixture(t *testing.T) (teamimport.Manifest, string) {
	t.Helper()
	base := t.TempDir()
	boot := filepath.Join(base, "cairn")
	for _, path := range []string{boot, filepath.Join(base, "scope"), filepath.Join(base, "team"), filepath.Join(base, "worktrees"), filepath.Join(boot, ".agents", "skills", "read-first", "scripts")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{"AGENTS.md": "# Cairn task charter\nRead the assigned task first.\n", ".agents/skills/read-first/SKILL.md": "Run scripts/read.sh.\n", ".agents/skills/read-first/scripts/read.sh": "#!/bin/sh\nprintf 'read-only'\n", "auth.json": "DO NOT COPY", "config.toml": "DO NOT COPY", "launch.sh": "DO NOT EXECUTE"} {
		if err := os.WriteFile(filepath.Join(boot, path), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m := teamimport.Manifest{Version: 1, NonSecretSources: true, TeamHome: filepath.Join(base, "team"), TeamSession: "owned-test", TetherCommand: "tether", WorktreesRoot: filepath.Join(base, "worktrees"), Path: "/owned/tools:/usr/bin", Roles: []teamimport.Role{{Name: "task-docs-1", Role: "task", Project: "docs", Scope: filepath.Join(base, "scope"), BootDir: boot, URN: "msg://agent/agent-mux/agt_original", Runtime: "codex", Model: "reviewed-model", Effort: "high", PermissionMode: "bypass", Prompt: "Read the team brief; this is an isolated fixture.", BaselineEvidence: "owned fake baseline, no live role selection", NativeScopes: []string{"session.write", "message.write", "catalog.write"}, MCPServers: []string{}, MCPTools: []string{}}}}
	return m, filepath.Join(base, "imported")
}

func TestImportedInputsResolveOriginalIdentityAndCompleteRoleAssets(t *testing.T) {
	m, root := fixture(t)
	if err := teamimport.Write(m, root); err != nil {
		t.Fatal(err)
	}
	catalog, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(root, "boot-profiles", m.Roles[0].Name+".yaml")
	profile, err := bootgen.LoadProfile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if profile.MCPServers == nil || profile.MCPTools == nil {
		t.Fatal("explicit empty grant became inheritance")
	}
	svc := &app.Service{CatalogRoot: root, Catalog: catalog}
	plan, err := svc.BuildLaunchPlan(app.CreateSessionInput{LaunchID: m.Roles[0].Name, BootProfileFile: profilePath})
	if err != nil {
		t.Fatal(err)
	}
	if plan.LogicalAgentID != "agt_original" || plan.Env["TEAM_URN"] != m.Roles[0].URN || plan.Env["TEAM_NAME"] != m.Roles[0].Name {
		t.Fatal("actor identity changed during supported launch resolution")
	}
	if plan.Env["TETHER_MCP_SERVERS"] != "" || plan.Env["TETHER_MCP_TOOLS"] != "[]" {
		t.Fatal("zero grant inherited an ambient/default tool surface")
	}
	if plan.Env["TMPDIR"] != filepath.Join(root, "tmp", m.Roles[0].Name) || plan.Env["GOTMPDIR"] != plan.Env["TMPDIR"] {
		t.Fatal("role temp escaped the owned catalog")
	}
	assets := map[string]bool{}
	for _, f := range plan.NativeFiles {
		assets[f.RelPath] = true
		if f.RelPath == ".agents/skills/read-first/scripts/read.sh" && f.Mode&0o100 == 0 {
			t.Fatal("skill executable mode lost")
		}
	}
	if !assets[".agents/skills/read-first/SKILL.md"] || !assets[".agents/skills/read-first/scripts/read.sh"] || assets["auth.json"] || assets["config.toml"] || assets["launch.sh"] {
		t.Fatal("complete safe skill tree was not preserved")
	}
	var prompt bytes.Buffer
	if err := bootgen.Generate(context.Background(), profile, root, &prompt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt.String(), "Cairn task charter") || !strings.HasPrefix(prompt.String(), "RUN CONTEXT: runtime-managed") || !strings.Contains(prompt.String(), m.Roles[0].Prompt) {
		t.Fatal("Cairn role or explicit runtime prompt missing")
	}
	provider := catalog.Providers["team-task-docs-1"]
	args := strings.Join(provider.Args, " ")
	for _, selection := range []string{`model="reviewed-model"`, `model_reasoning_effort="high"`, "shell_environment_policy.set=", m.TeamHome, m.WorktreesRoot, m.Roles[0].Scope, "sandbox_workspace_write.writable_roots="} {
		if !strings.Contains(args, selection) {
			t.Fatalf("effective provider selection/scope missing: %s", selection)
		}
	}
	if strings.Contains(args, "--add-dir") {
		t.Fatal("interactive-only directory flags passed to app-server")
	}
	if _, err := os.Stat(filepath.Join(root, "state", "tether.db")); !os.IsNotExist(err) {
		t.Fatal("offline conversion opened a daemon store")
	}
}

func TestImportedTeamHelperUsesOwnedCatalogAndInheritedAuth(t *testing.T) {
	m, root := fixture(t)
	// Only this fake CLI executes: no daemon, provider or credential helper.
	m.TetherCommand = filepath.Join(filepath.Dir(root), "fake tether's cli")
	if err := os.WriteFile(m.TetherCommand, []byte("#!/bin/sh\n[ \"$TETHER_TOKEN\" = owned-fixture-auth ] || exit 7\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := teamimport.Write(m, root); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := (&app.Service{CatalogRoot: root, Catalog: c}).BuildLaunchPlan(app.CreateSessionInput{LaunchID: m.Roles[0].Name})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := plan.Env["TEAM_TETHER"]
	if wrapper != filepath.Join(root, "bin", "team-tether") {
		t.Fatal("team helper escaped the owned catalog")
	}
	args := []string{"messages", "list", "literal ' $(not-a-command)"}
	cmd := exec.CommandContext(context.Background(), wrapper, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TETHER_TOKEN=owned-fixture-auth"}
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(append([]string{"--catalog", root}, args...), "\n") + "\n"
	if string(output) != want {
		t.Fatal("wrapper changed argument boundaries or omitted the owned catalog")
	}
	for _, override := range [][]string{{"--catalog", "/shared/catalog", "messages", "list"}, {"messages", "list", "--catalog=/shared/catalog"}} {
		cmd := exec.CommandContext(context.Background(), wrapper, override...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "TETHER_TOKEN=owned-fixture-auth"}
		if output, err := cmd.Output(); err == nil || len(output) != 0 {
			t.Fatal("helper permitted a shared-catalog override or executed the fake CLI")
		}
	}
	info, err := os.Stat(wrapper)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("owned helper must be private and executable")
	}
	providerArgs := strings.Join(c.Providers["team-"+m.Roles[0].Name].Args, " ")
	if !strings.Contains(providerArgs, wrapper) {
		t.Fatal("provider shell policy did not receive the same helper")
	}
}

func TestExplicitMCPGrantRemainsExact(t *testing.T) {
	m, root := fixture(t)
	m.Roles[0].MCPServers = []string{"owned-read"}
	m.Roles[0].MCPTools = []string{"fixture_read"}
	m.MCP = []teamimport.Server{{ID: "owned-read", Transport: "stdio", Command: "/owned/fake-mcp", TokenFile: "/owned/private/credential"}}
	if err := teamimport.Write(m, root); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateLaunchMCPGrants(m.Roles[0].Name); err != nil {
		t.Fatal(err)
	}
	p, err := bootgen.LoadProfile(filepath.Join(root, "boot-profiles", m.Roles[0].Name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.MCPServers, m.Roles[0].MCPServers) || !reflect.DeepEqual(p.MCPTools, m.Roles[0].MCPTools) {
		t.Fatal("explicit baseline grant changed")
	}
	// Omitting --boot-profile must preserve the same nonempty server grant
	// while the project has no shared grant that can override another role.
	svc := &app.Service{CatalogRoot: root, Catalog: c}
	plan, err := svc.BuildLaunchPlan(app.CreateSessionInput{LaunchID: m.Roles[0].Name})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Env["TETHER_MCP_SERVERS"] != "owned-read" || plan.Env["TETHER_MCP_TOOLS"] != `["fixture_read"]` {
		t.Fatalf("launch without boot profile grant: servers=%q tools=%q", plan.Env["TETHER_MCP_SERVERS"], plan.Env["TETHER_MCP_TOOLS"])
	}
}

func TestInvalidInputsLeaveDestinationAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*teamimport.Manifest)
	}{
		{"missing grant", func(m *teamimport.Manifest) { m.Roles[0].MCPServers = nil }},
		{"missing tool grant", func(m *teamimport.Manifest) { m.Roles[0].MCPTools = nil }},
		{"missing native baseline", func(m *teamimport.Manifest) { m.Roles[0].NativeScopes = nil }},
		{"missing model", func(m *teamimport.Manifest) { m.Roles[0].Model = "" }},
		{"missing baseline", func(m *teamimport.Manifest) { m.Roles[0].BaselineEvidence = "" }},
		{"unsupported runtime", func(m *teamimport.Manifest) { m.Roles[0].Runtime = "claude" }},
		{"missing server mapping", func(m *teamimport.Manifest) { m.Roles[0].MCPServers = []string{"ambient"} }},
		{"literal credential", func(m *teamimport.Manifest) { m.MCP = []teamimport.Server{{ID: "bad", Token: "owned-fake-secret"}} }},
		{"bad identity", func(m *teamimport.Manifest) { m.Roles[0].URN = "msg://user/local/operator" }},
		{"unreviewed assets", func(m *teamimport.Manifest) { m.NonSecretSources = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, root := fixture(t)
			tc.edit(&m)
			if err := teamimport.Write(m, root); err == nil {
				t.Fatal("invalid baseline accepted")
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Fatal("invalid conversion changed output")
			}
		})
	}
}

func TestTwoRolesInSameProjectKeepIndependentGrants(t *testing.T) {
	m, root := fixture(t)
	m.Roles[0].MCPServers = []string{"owned-read"}
	m.Roles[0].MCPTools = []string{"fixture_read"}
	m.MCP = []teamimport.Server{{ID: "owned-read", Transport: "stdio", Command: "/owned/fake-mcp"}}
	second := m.Roles[0]
	second.Name, second.URN = "task-docs-2", "msg://agent/agent-mux/agt_second"
	second.MCPServers, second.MCPTools = []string{}, []string{}
	m.Roles = append(m.Roles, second)
	if err := teamimport.Write(m, root); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	svc := &app.Service{CatalogRoot: root, Catalog: c}
	for _, r := range m.Roles {
		plan, err := svc.BuildLaunchPlan(app.CreateSessionInput{LaunchID: r.Name})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Env["TETHER_MCP_SERVERS"] != strings.Join(r.MCPServers, ",") {
			t.Fatal("one role inherited another role's server grant")
		}
	}
}

func TestRefusesSymlinkAssetAndExistingOrSharedOutput(t *testing.T) {
	m, root := fixture(t)
	if err := teamimport.Write(m, root+" split"); err == nil {
		t.Fatal("team helper would split the output command path")
	}
	link := filepath.Join(m.Roles[0].BootDir, ".agents", "skills", "read-first", "outside")
	if err := os.Symlink(filepath.Join(m.Roles[0].BootDir, "auth.json"), link); err != nil {
		t.Fatal(err)
	}
	if err := teamimport.Write(m, root); err == nil {
		t.Fatal("symlink source accepted")
	}
	if err := teamimport.Write(m, filepath.Join(m.TeamHome, "imported")); err == nil {
		t.Fatal("shared team output accepted")
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "retain")
	if err := os.WriteFile(sentinel, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := teamimport.Write(m, root); err == nil {
		t.Fatal("existing output accepted")
	}
	b, err := os.ReadFile(sentinel)
	if err != nil || string(b) != "original" {
		t.Fatal("existing tree changed")
	}
}
