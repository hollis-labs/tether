//go:build smoke

// Package mcpadapter smoke tests require hadrond on PATH and a populated
// ~/.agent-mux/catalog/mcp-servers/hadron.yaml. Run with:
//
//	go test -tags smoke -v -run TestProxySmoke ./internal/mcpadapter/
package mcpadapter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/hollis-labs/tether/internal/config"
)

// TestProxySmokeHadronHealth is the Phase 1 exit-gate smoke test.
// It:
//  1. Loads mcp-servers from the real catalog
//  2. Starts a ClientPool (spawns hadrond mcp as a subprocess)
//  3. Calls hadron_health through the ProxyRouter
//  4. Asserts the result is non-error and contains expected fields
func TestProxySmokeHadronHealth(t *testing.T) {
	// Require hadrond on PATH.
	hadrondPath, err := exec.LookPath("hadrond")
	if err != nil {
		t.Skipf("hadrond not on PATH: %v", err)
	}
	t.Logf("hadrond: %s", hadrondPath)

	catalogDir := filepath.Join(os.Getenv("HOME"), ".agent-mux", "catalog")
	if _, err := os.Stat(filepath.Join(catalogDir, "mcp-servers")); os.IsNotExist(err) {
		t.Skipf("catalog mcp-servers dir not found at %s", catalogDir)
	}

	entries, err := config.LoadMCPServers(catalogDir)
	if err != nil {
		t.Fatalf("LoadMCPServers: %v", err)
	}
	if len(entries) == 0 {
		t.Skip("no mcp-servers entries found — skipping smoke test")
	}
	t.Logf("loaded %d mcp-server entries", len(entries))

	reg := NewToolRegistry()
	pool := NewClientPool(entries, reg)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool.Start: %v", err)
	}
	defer pool.Shutdown()

	// ── Check 1: hadron_* tools are registered ────────────────────────────────
	defs := reg.AllDefinitions()
	var hadronTools []string
	for _, d := range defs {
		if len(d.Name) >= 7 && d.Name[:7] == "hadron_" {
			hadronTools = append(hadronTools, d.Name)
		}
	}
	if len(hadronTools) == 0 {
		t.Fatal("no hadron_* tools registered — proxy did not connect to hadrond")
	}
	t.Logf("hadron tools registered: %d (sample: %v)", len(hadronTools), hadronTools[:min(3, len(hadronTools))])

	// ── Check 2: hadron_health is present ────────────────────────────────────
	rt, ok := reg.Lookup("hadron_health")
	if !ok {
		t.Fatal("hadron_health not found in registry")
	}
	if rt.Client == nil {
		t.Fatal("hadron_health has nil client — upstream startup failed")
	}

	// ── Check 3: ProxyRouter forwards hadron_health and gets a real result ────
	router := NewProxyRouter(reg)
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "hadron_health"},
	}
	result, err := router.Handle(ctx, req)
	if err != nil {
		t.Fatalf("router.Handle(hadron_health): %v", err)
	}
	if result.IsError {
		t.Fatalf("hadron_health returned IsError:true — upstream call failed")
	}
	if len(result.Content) == 0 {
		t.Fatal("hadron_health returned empty content")
	}
	t.Logf("hadron_health result: %+v", result.Content[0])

	// ── Check 4: StatusSummary shows hadron as connected ──────────────────────
	statuses := pool.StatusSummary()
	var hadronStatus *ServerStatus
	for i := range statuses {
		if statuses[i].ID == "hadron" {
			hadronStatus = &statuses[i]
			break
		}
	}
	if hadronStatus == nil {
		t.Fatal("hadron not found in StatusSummary")
	}
	if hadronStatus.Status != "connected" {
		t.Fatalf("hadron status = %q, want %q (error: %s)", hadronStatus.Status, "connected", hadronStatus.Error)
	}
	if hadronStatus.ToolCount == 0 {
		t.Fatal("hadron ToolCount = 0")
	}
	t.Logf("hadron status: %s, tool_count: %d", hadronStatus.Status, hadronStatus.ToolCount)

	t.Log("==> All proxy smoke checks passed")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
