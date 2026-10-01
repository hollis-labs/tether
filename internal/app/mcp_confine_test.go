package app

// CW-20261001-0227: a launched agent's planted proxy is confined to the
// upstreams it was granted -- torque and tesseract unless a launch, project or
// boot profile names its own list.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func findPlanted(t *testing.T, root, name string) string {
	t.Helper()
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Base(path) == name {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no %s planted under %s", name, root)
	}
	return found
}

// plantFor plants a session's launch for a plan and returns the boot dir,
// with the planted proxy's argv and env as a session launch wires them.
func plantFor(t *testing.T, brand, providerID, runtimeKind string, env map[string]string) (bootDir string) {
	t.Helper()
	svc := &Service{CatalogRoot: t.TempDir(), Catalog: &config.Catalog{Global: config.Global{Version: "test"}}}
	ws, repo := t.TempDir(), t.TempDir()
	plan := &launch.Plan{
		LaunchID: "demo", ProjectID: "project", LogicalAgentID: "agent",
		ProviderID: providerID, ProviderBrand: brand, RuntimeKind: runtimeKind,
		RepoRoot: repo, WriteHome: ws, WorkspaceMode: "shared", Command: brand,
		BootPrompt: testBootPrompt, Env: env,
	}
	prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{
		TetherCommand: "tether",
		TetherArgs:    TetherMCPPlant("/catalog", "sess-1", false).Args,
		TetherEnv:     tetherEnvMap(plan.Env),
	})
	if err != nil {
		t.Fatalf("prepareSharedLaunch: %v", err)
	}
	return prepared.PlantedBootDir
}

func TestTetherMCPPlant_ConfinesOnlySessionProxies(t *testing.T) {
	sess := TetherMCPPlant("/catalog", "sess-1", false).Args
	if !slices.Contains(sess, "--confine") {
		t.Fatalf("a session's proxy is not confined: %v", sess)
	}
	if slices.Index(sess, "--confine") > slices.Index(sess, "--session") {
		t.Fatalf("--confine should precede --session: %v", sess)
	}
	// `tether boot` plants for the operator's own terminal, with no session.
	if boot := TetherMCPPlant("/catalog", "", false).Args; slices.Contains(boot, "--confine") {
		t.Fatalf("the operator's boot-exec proxy is confined: %v", boot)
	}
}

func TestTetherEnvMap_DefaultAndExplicit(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no env: the default", nil, "torque,tesseract"},
		{"no list: the default", map[string]string{"X": "y"}, "torque,tesseract"},
		{"explicit list replaces the default", map[string]string{"TETHER_MCP_SERVERS": "loom"}, "loom"},
		{"an explicit list can include more", map[string]string{"TETHER_MCP_SERVERS": "torque,tesseract,nanite"}, "torque,tesseract,nanite"},
	} {
		got := tetherEnvMap(tc.env)
		if len(got) != 1 || got["TETHER_MCP_SERVERS"] != tc.want {
			t.Fatalf("%s: tetherEnvMap = %v; want only TETHER_MCP_SERVERS=%s", tc.name, got, tc.want)
		}
	}
	if got := tetherEnvMap(nil)["TETHER_MCP_SERVERS"]; strings.Contains(got, "cerberus") {
		t.Fatalf("cerberus is in the default: %q", got)
	}
}

// The file Claude actually reads.
func TestPlantedClaudeMCPJSON_CarriesConfineAndAllowList(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"default", nil, "torque,tesseract"},
		{"explicit", map[string]string{"TETHER_MCP_SERVERS": "torque,loom"}, "torque,loom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bootDir := plantFor(t, "claude", "claude-code", config.RuntimeKindStreamingStdio, tc.env)
			data, err := os.ReadFile(findPlanted(t, bootDir, ".mcp.json"))
			if err != nil {
				t.Fatal(err)
			}
			var planted struct {
				MCPServers map[string]struct {
					Args []string          `json:"args"`
					Env  map[string]string `json:"env"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(data, &planted); err != nil {
				t.Fatal(err)
			}
			if len(planted.MCPServers) != 1 {
				t.Fatalf("planted %d MCP servers, want exactly the tether proxy: %s", len(planted.MCPServers), data)
			}
			tether, ok := planted.MCPServers["tether"]
			if !ok {
				t.Fatalf("no tether entry: %s", data)
			}
			if !slices.Contains(tether.Args, "--confine") || !slices.Contains(tether.Args, "--proxy") {
				t.Fatalf("planted tether args %v lack --proxy --confine", tether.Args)
			}
			if got := tether.Env["TETHER_MCP_SERVERS"]; got != tc.want {
				t.Fatalf("planted TETHER_MCP_SERVERS = %q; want %q", got, tc.want)
			}
		})
	}
}

// codex reads its MCP servers from the planted config.toml.
func TestPlantedCodexConfig_CarriesConfineAndAllowList(t *testing.T) {
	bootDir := plantFor(t, "codex", "codex-cli", config.RuntimeKindSubprocess, nil)
	data, err := os.ReadFile(findPlanted(t, bootDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(data)
	for _, want := range []string{`"--confine"`, `"TETHER_MCP_SERVERS" = "torque,tesseract"`} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("planted config.toml lacks %s:\n%s", want, cfg)
		}
	}
}

// A launched session's planted server is BOTH confined to its granted upstreams
// (CW-20261001-0227) and daemon-only (CW-20261001-0173). The two were written
// independently and their combination is what keeps an agent from reaching an
// upstream it was not granted while the state directory stays protected: a merge
// that drops either flag must fail here, and the boot-exec path, which plants
// for the operator's own terminal, must carry neither.
func TestTetherMCPPlant_LaunchedSessionIsConfinedAndDaemonOnly(t *testing.T) {
	sess := TetherMCPPlant("/catalog", "sess-1", false).Args
	for _, want := range []string{"--proxy", "--confine", "--daemon-only", "--session", "sess-1"} {
		if !slices.Contains(sess, want) {
			t.Fatalf("a launched session's planted argv lacks %q: %v", want, sess)
		}
	}
	boot := TetherMCPPlant("/catalog", "", false).Args
	for _, bad := range []string{"--confine", "--daemon-only"} {
		if slices.Contains(boot, bad) {
			t.Fatalf("boot-exec plants for the operator's own terminal and must not carry %s: %v", bad, boot)
		}
	}
}
