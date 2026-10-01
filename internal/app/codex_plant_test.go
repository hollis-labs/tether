package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentlaunch"
	permission "github.com/hollis-labs/go-permission"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// The config.toml planted into a codex boot dir states the posture the launch
// runs under, not the go-providers adapter's headless default (CW-20261001-0216).
// Before, the file said approval_policy "never" while the launch's argv said
// "on-request"; a codex started from the boot dir without the argv would have
// refused every MCP tool call.
func TestPlantedCodexConfig_CarriesThePosture(t *testing.T) {
	for _, tc := range []struct {
		name        string
		providerID  string
		runtimeKind string
		mode        string // Tether permission mode
	}{
		{"exec, default mode", "codex-cli", config.RuntimeKindSubprocess, config.PermissionModeDefault},
		{"exec, bypass mode", "codex-cli", config.RuntimeKindSubprocess, config.PermissionModeBypass},
		{"app-server, default mode", "codex-app-server", config.RuntimeKindJSONRPCStdio, config.PermissionModeDefault},
		{"app-server, bypass mode", "codex-app-server", config.RuntimeKindJSONRPCStdio, config.PermissionModeBypass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &Service{
				CatalogRoot: t.TempDir(),
				Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
			}
			ws, repo := t.TempDir(), t.TempDir()
			plan := &launch.Plan{
				LaunchID: "demo", ProjectID: "project", LogicalAgentID: "agent",
				ProviderID: tc.providerID, ProviderBrand: "codex", RuntimeKind: tc.runtimeKind,
				RepoRoot: repo, WriteHome: ws, WorkspaceMode: "shared",
				Command: "codex", PermissionMode: tc.mode, BootPrompt: testBootPrompt,
			}
			prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{
				TetherCommand: "tether", TetherArgs: []string{"mcp"},
			})
			if err != nil {
				t.Fatalf("prepareSharedLaunch: %v", err)
			}
			b, err := os.ReadFile(filepath.Join(prepared.PlantedBootDir, "config.toml"))
			if err != nil {
				t.Fatalf("planted config.toml: %v", err)
			}
			text := string(b)
			// Both modes map to accept-edits (go-providers' codex posture): the
			// agent writes its workspace, and codex ASKS rather than refusing, which
			// is what lets its MCP tool calls be answered (codex_approval.go).
			for _, want := range []string{`approval_policy = "on-request"`, `sandbox_mode = "workspace-write"`} {
				if !strings.Contains(text, want) {
					t.Errorf("planted config.toml lacks %s:\n%s", want, text)
				}
			}
			if strings.Contains(text, `approval_policy = "never"`) {
				t.Errorf("planted config.toml still says approval_policy never:\n%s", text)
			}
		})
	}
}

// The registry's posture arguments are the source, and a missing or unmapped
// posture leaves the adapter's own default rather than failing the launch.
func TestCodexPostureSettings(t *testing.T) {
	if sb, ap := codexPostureSettings(nil); sb != "" || ap != "" {
		t.Fatalf("nil plan: %q %q", sb, ap)
	}
	plan := func(provider string, perm permission.Mode, rt runtimes.Mode) *agentlaunch.LaunchPlan {
		p := &agentlaunch.LaunchPlan{Runtime: rt}
		p.Provider.ID = provider
		p.Provider.Permission = perm
		return p
	}
	for name, tc := range map[string]struct {
		plan             *agentlaunch.LaunchPlan
		wantSB, wantAppr string
	}{
		"accept-edits":   {plan("codex", permission.ModeAcceptEdits, runtimes.ModeSubprocessPerTurn), "workspace-write", "on-request"},
		"default":        {plan("codex", permission.ModeDefault, runtimes.ModeSubprocessPerTurn), "read-only", "on-request"},
		"plan":           {plan("codex", permission.ModePlan, runtimes.ModeJSONRPCStdio), "read-only", "never"},
		"yolo":           {plan("codex", permission.ModeYolo, runtimes.ModeJSONRPCStdio), "danger-full-access", "never"},
		"no posture":     {plan("codex", "", runtimes.ModeSubprocessPerTurn), "", ""},
		"unknown id":     {plan("no-such-runtime", permission.ModeAcceptEdits, runtimes.ModeSubprocessPerTurn), "", ""},
		"invalid":        {plan("codex", permission.Mode("sudo"), runtimes.ModeSubprocessPerTurn), "", ""},
		"acp has no map": {plan("codex", permission.ModeAcceptEdits, runtimes.ModeACPStdio), "", ""},
	} {
		sb, ap := codexPostureSettings(tc.plan)
		if sb != tc.wantSB || ap != tc.wantAppr {
			t.Errorf("%s: got sandbox %q approval %q, want %q %q", name, sb, ap, tc.wantSB, tc.wantAppr)
		}
	}
}
