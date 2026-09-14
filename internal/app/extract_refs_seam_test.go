package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/store"
)

func TestLaunchConfigSeam_PlantedMCPJSON(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extractRefs bool
		wantFlag    bool
		wantAttr    string
	}{
		{
			name:        "extraction on plants --extract-refs and sets proxy attribution",
			extractRefs: true,
			wantFlag:    true,
			wantAttr:    store.RefAttributionProxy,
		},
		{
			name:        "extraction off (default) omits --extract-refs and sets none attribution",
			extractRefs: false,
			wantFlag:    false,
			wantAttr:    store.RefAttributionNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &Service{
				CatalogRoot: t.TempDir(),
				Catalog: &config.Catalog{
					Global: config.Global{Version: "test"},
				},
			}
			wsRoot := t.TempDir()
			sessionID := "sess-test-" + tc.wantAttr
			plan := &launch.Plan{
				LaunchID:       "demo",
				ProjectID:      "project",
				LogicalAgentID: "agent",
				ProviderID:     "claude-pty",
				ProviderBrand:  "claude",
				RuntimeKind:    config.RuntimeKindPTY,
				RepoRoot:       t.TempDir(),
				WriteHome:      wsRoot,
				WorkspaceMode:  "worktree",
				Command:        "claude",
				BootPrompt:     "boot",
				BootMode:       "stdin",
				ExtractRefs:    tc.extractRefs,
			}

			mcpPlan := MuxMCPPlant(svc.CatalogRoot, sessionID, plan.ExtractRefs)
			if mcpPlan.Attribution != tc.wantAttr {
				t.Fatalf("mcpPlan.Attribution = %q, want %q", mcpPlan.Attribution, tc.wantAttr)
			}

			prepared, err := svc.prepareSharedLaunch(context.Background(), plan, wsRoot, plantContextInput{
				MuxCommand: muxCommandPath(),
				MuxArgs:    mcpPlan.Args,
			})
			if err != nil {
				t.Fatalf("prepareSharedLaunch: %v", err)
			}

			// Read the ACTUAL planted .mcp.json from disk, per CW-20260912-0112 acceptance criteria.
			mcpPath := filepath.Join(prepared.PlantedBootDir, ".mcp.json")
			data, err := os.ReadFile(mcpPath)
			if err != nil {
				t.Fatalf("read planted .mcp.json at %s: %v", mcpPath, err)
			}

			var planted struct {
				MCPServers map[string]struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(data, &planted); err != nil {
				t.Fatalf("unmarshal planted .mcp.json: %v\ncontent:\n%s", err, string(data))
			}

			// Find tether/mux server in mcpServers
			var plantedArgs []string
			for _, srv := range planted.MCPServers {
				if slices.Contains(srv.Args, "mcp") && slices.Contains(srv.Args, "--proxy") {
					plantedArgs = srv.Args
					break
				}
			}
			if plantedArgs == nil {
				t.Fatalf("could not find tether proxy server in planted .mcp.json:\n%s", string(data))
			}

			hasExtractFlag := slices.Contains(plantedArgs, "--extract-refs")
			if hasExtractFlag != tc.wantFlag {
				t.Errorf("--extract-refs in planted .mcp.json = %v, want %v (args: %v)", hasExtractFlag, tc.wantFlag, plantedArgs)
			}

			hasSession := slices.Contains(plantedArgs, "--session")
			if !hasSession {
				t.Errorf("--session missing from planted .mcp.json args: %v", plantedArgs)
			}
		})
	}
}

func TestLaunchSession_ExtractRefsRecordsAttribution(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extractRefs bool
		wantAttr    string
		wantFlag    bool
	}{
		{
			name:        "extract refs on sets proxy attribution in store",
			extractRefs: true,
			wantAttr:    store.RefAttributionProxy,
			wantFlag:    true,
		},
		{
			name:        "extract refs off sets none attribution in store",
			extractRefs: false,
			wantAttr:    store.RefAttributionNone,
			wantFlag:    false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}

			catDir := t.TempDir()
			wsRoot := t.TempDir()
			sessID := "sess-" + tc.wantAttr

			svc := &Service{
				CatalogRoot: catDir,
				Catalog: &config.Catalog{
					Global: config.Global{Version: "test"},
				},
				Store: db,
			}

			row := store.SessionRow{
				ID:             sessID,
				LaunchID:       "launch-1",
				ProjectID:      "proj-1",
				LogicalAgentID: "agent-1",
				ProviderID:     "claude-pty",
				ProviderKind:   "cli",
				Workspace:      wsRoot,
				State:          "created",
			}
			plan := &launch.Plan{
				LaunchID:       "launch-1",
				ProjectID:      "proj-1",
				LogicalAgentID: "agent-1",
				ProviderID:     "claude-pty",
				ProviderBrand:  "claude",
				RuntimeKind:    config.RuntimeKindPTY,
				RepoRoot:       t.TempDir(),
				WriteHome:      wsRoot,
				WorkspaceMode:  "shared",
				Command:        "echo",
				BootPrompt:     "boot",
				BootMode:       "stdin",
				ExtractRefs:    tc.extractRefs,
			}

			if err := db.CreateSession(row, plan); err != nil {
				t.Fatalf("create session: %v", err)
			}

			// Call s.LaunchSession with stub runtime factory
			svc.factories = map[string]RuntimeFactory{
				"claude-pty": func(_ *launch.Plan) (agentsessions.Runtime, error) {
					return stub.New(plan)
				},
			}
			svc.Manager = agentsessions.NewManager(stateSinkAdapter{db: db})

			launched, err := svc.LaunchSession(sessID)
			if err != nil {
				t.Fatalf("LaunchSession: %v", err)
			}
			if launched == nil {
				t.Fatal("launched is nil")
			}

			// Verify attribution recorded in store
			savedRow, err := db.GetSession(sessID)
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if !savedRow.RefAttribution.Valid || savedRow.RefAttribution.String != tc.wantAttr {
				t.Errorf("saved RefAttribution = %v (valid=%v), want %q",
					savedRow.RefAttribution.String, savedRow.RefAttribution.Valid, tc.wantAttr)
			}

			// Verify planted .mcp.json on disk
			mcpFile := filepath.Join(wsRoot, "boot", "claude", ".mcp.json")
			// Look for any .mcp.json under wsRoot/boot
			var foundMCP string
			_ = filepath.Walk(filepath.Join(wsRoot, "boot"), func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() && filepath.Base(path) == ".mcp.json" {
					foundMCP = path
				}
				return nil
			})
			if foundMCP == "" {
				t.Fatalf("could not find planted .mcp.json in %s", filepath.Join(wsRoot, "boot"))
			}

			data, err := os.ReadFile(foundMCP)
			if err != nil {
				t.Fatalf("read planted .mcp.json: %v", err)
			}
			var planted struct {
				MCPServers map[string]struct {
					Args []string `json:"args"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(data, &planted); err != nil {
				t.Fatalf("unmarshal .mcp.json: %v", err)
			}

			var plantedArgs []string
			for _, srv := range planted.MCPServers {
				if slices.Contains(srv.Args, "mcp") && slices.Contains(srv.Args, "--proxy") {
					plantedArgs = srv.Args
					break
				}
			}
			if plantedArgs == nil {
				t.Fatalf("proxy args not found in %s:\n%s", mcpFile, string(data))
			}

			hasFlag := slices.Contains(plantedArgs, "--extract-refs")
			if hasFlag != tc.wantFlag {
				t.Errorf(".mcp.json --extract-refs = %v, want %v (args: %v)", hasFlag, tc.wantFlag, plantedArgs)
			}
		})
	}
}
