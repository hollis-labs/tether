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
		MuxCommand: "mux",
		MuxArgs:    MuxMCPPlant("/catalog", "sess-1", false).Args,
		MuxEnv:     muxEnvMap(plan.Env),
	})
	if err != nil {
		t.Fatalf("prepareSharedLaunch: %v", err)
	}
	return prepared.PlantedBootDir
}

func TestMuxMCPPlant_ConfinesOnlySessionProxies(t *testing.T) {
	sess := MuxMCPPlant("/catalog", "sess-1", false).Args
	if !slices.Contains(sess, "--confine") {
		t.Fatalf("a session's proxy is not confined: %v", sess)
	}
	if slices.Index(sess, "--confine") > slices.Index(sess, "--session") {
		t.Fatalf("--confine should precede --session: %v", sess)
	}
	// `mux boot` plants for the operator's own terminal, with no session.
	if boot := MuxMCPPlant("/catalog", "", false).Args; slices.Contains(boot, "--confine") {
		t.Fatalf("the operator's boot-exec proxy is confined: %v", boot)
	}
}

func TestMuxEnvMap_DefaultAndExplicit(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no env: the default", nil, "torque,tesseract"},
		{"no list: the default", map[string]string{"X": "y"}, "torque,tesseract"},
		{"explicit list replaces the default", map[string]string{"MUX_MCP_SERVERS": "loom"}, "loom"},
		{"an explicit list can include more", map[string]string{"MUX_MCP_SERVERS": "torque,tesseract,nanite"}, "torque,tesseract,nanite"},
	} {
		got := muxEnvMap(tc.env)
		if len(got) != 1 || got["MUX_MCP_SERVERS"] != tc.want {
			t.Fatalf("%s: muxEnvMap = %v; want only MUX_MCP_SERVERS=%s", tc.name, got, tc.want)
		}
	}
	if got := muxEnvMap(nil)["MUX_MCP_SERVERS"]; strings.Contains(got, "cerberus") {
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
		{"explicit", map[string]string{"MUX_MCP_SERVERS": "torque,loom"}, "torque,loom"},
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
				t.Fatalf("planted %d MCP servers, want exactly the mux proxy: %s", len(planted.MCPServers), data)
			}
			mux, ok := planted.MCPServers["mux"]
			if !ok {
				t.Fatalf("no mux entry: %s", data)
			}
			if !slices.Contains(mux.Args, "--confine") || !slices.Contains(mux.Args, "--proxy") {
				t.Fatalf("planted mux args %v lack --proxy --confine", mux.Args)
			}
			if got := mux.Env["MUX_MCP_SERVERS"]; got != tc.want {
				t.Fatalf("planted MUX_MCP_SERVERS = %q; want %q", got, tc.want)
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
	for _, want := range []string{`"--confine"`, `"MUX_MCP_SERVERS" = "torque,tesseract"`} {
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
func TestMuxMCPPlant_LaunchedSessionIsConfinedAndDaemonOnly(t *testing.T) {
	sess := MuxMCPPlant("/catalog", "sess-1", false).Args
	for _, want := range []string{"--proxy", "--confine", "--daemon-only", "--session", "sess-1"} {
		if !slices.Contains(sess, want) {
			t.Fatalf("a launched session's planted argv lacks %q: %v", want, sess)
		}
	}
	boot := MuxMCPPlant("/catalog", "", false).Args
	for _, bad := range []string{"--confine", "--daemon-only"} {
		if slices.Contains(boot, bad) {
			t.Fatalf("boot-exec plants for the operator's own terminal and must not carry %s: %v", bad, boot)
		}
	}
}
