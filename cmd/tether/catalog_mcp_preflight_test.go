package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
)

func TestCatalogValidatePreflightsCandidateRunningMCPGrants(t *testing.T) {
	root := filepath.Join(t.TempDir(), "catalog")
	src, _ := filepath.Abs("../../examples/catalog")
	if out, err := exec.Command("cp", "-r", src, root).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	serverPath := filepath.Join(root, "mcp-servers", "test-upstream.yaml")
	if err := os.MkdirAll(filepath.Dir(serverPath), 0755); err != nil {
		t.Fatal(err)
	}
	body := "id: test-upstream\ntransport: http\nurl: http://localhost:12345/mcp\n"
	if err := os.WriteFile(serverPath, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	cat, err := config.LoadLayered(root)
	if err != nil {
		t.Fatal(err)
	}
	stateDB := filepath.Join(t.TempDir(), "state.db")
	cat.Global.Catalog.Defaults.StateDB = stateDB
	db, err := store.Open(stateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateSession(store.SessionRow{ID: "affected", LogicalAgentID: "a", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSessionMCPPolicy(context.Background(), mcpgateway.SessionPolicy{SessionID: "affected", AgentID: "a", Servers: []string{"test-upstream"}}.Seal()); err != nil {
		t.Fatal(err)
	}
	svc := &app.Service{Catalog: cat, CatalogRoot: root, Store: db}
	engine := newCatalogApplyEngine(svc)
	for _, tc := range []struct {
		reason string
		file   catalogapply.File
	}{
		{"removed", catalogapply.File{Path: "mcp-servers/test-upstream.yaml", Delete: true}},
		{"disabled", catalogapply.File{Path: "mcp-servers/test-upstream.yaml", Content: body + "enabled: false\n"}},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			report, err := engine.ValidateBundle([]catalogapply.File{tc.file}, catalogapply.Options{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, result := range report.Results {
				if result.Name == "running session MCP grants" {
					found = true
					if result.Status != "warn" || result.Entity != "session:affected" || !strings.Contains(result.Message, tc.reason) || !strings.Contains(result.Message, "test-upstream") || !strings.Contains(result.Remedy, "relaunch") {
						t.Fatalf("warning=%+v", result)
					}
				}
			}
			if !found || report.Warnings == 0 {
				t.Fatalf("no preflight: %+v", report)
			}
			current, err := config.LoadLayered(root)
			if err != nil || !current.MCPServerEnabled["test-upstream"] {
				t.Fatal("validation changed live catalog", err)
			}
		})
	}
}
