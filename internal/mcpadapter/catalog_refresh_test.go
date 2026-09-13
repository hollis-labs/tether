package mcpadapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
)

func TestCatalogReadsRefreshAfterValidEdit(t *testing.T) {
	root := newCatalogReadFixture(t, "old")
	initial, err := config.LoadLayered(root)
	if err != nil {
		t.Fatalf("load initial catalog: %v", err)
	}
	a := New(&app.Service{CatalogRoot: root, Catalog: initial}, "", nil)

	assertCatalogReadIDs(t, a, "old")
	writeCatalogGeneration(t, root, "new")
	assertCatalogReadIDs(t, a, "new")

	if _, ok := a.svc.Catalog.Launches["old-launch"]; !ok {
		t.Fatal("MCP read replaced the service startup catalog")
	}
	if _, ok := a.svc.Catalog.Launches["new-launch"]; ok {
		t.Fatal("MCP read mutated the service startup catalog")
	}

	health, err := a.handleHealth(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	body := parseToolJSON(t, health)
	if got := body["ok"]; got != true {
		t.Fatalf("health ok = %v, want true", got)
	}
	assertCatalogObservation(t, body)
}

func TestCatalogReadReportsMalformedEditAndRecovers(t *testing.T) {
	root := newCatalogReadFixture(t, "good")
	initial, err := config.LoadLayered(root)
	if err != nil {
		t.Fatalf("load initial catalog: %v", err)
	}
	a := New(&app.Service{CatalogRoot: root, Catalog: initial}, "", nil)

	writeFile(t, filepath.Join(root, "launches", "good.yaml"), "id: [\n")
	result, err := a.handleListLaunches(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("list launches: %v", err)
	}
	if !result.IsError {
		t.Fatal("malformed catalog read did not return an MCP tool error")
	}
	body := parseToolJSON(t, result)
	if got := body["code"]; got != "catalog_reload_failed" {
		t.Fatalf("error code = %v, want catalog_reload_failed", got)
	}

	health, err := a.handleHealth(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("health during malformed edit: %v", err)
	}
	healthBody := parseToolJSON(t, health)
	if got := healthBody["ok"]; got != false {
		t.Fatalf("health ok = %v, want false", got)
	}
	read, ok := healthBody["catalog_read"].(map[string]any)
	if !ok {
		t.Fatalf("catalog_read = %#v, want object", healthBody["catalog_read"])
	}
	if got := read["status"]; got != "reload_failed" {
		t.Fatalf("catalog status = %v, want reload_failed", got)
	}
	if read["error"] == "" {
		t.Fatal("catalog reload failure omitted error detail")
	}
	if _, exists := healthBody["launches"]; exists {
		t.Fatal("health exposed stale launch counts after catalog reload failure")
	}

	writeCatalogGeneration(t, root, "recovered")
	assertCatalogReadIDs(t, a, "recovered")
}

func TestCatalogReadsAreSafeConcurrently(t *testing.T) {
	root := newCatalogReadFixture(t, "stable")
	initial, err := config.LoadLayered(root)
	if err != nil {
		t.Fatalf("load initial catalog: %v", err)
	}
	a := New(&app.Service{CatalogRoot: root, Catalog: initial}, "", nil)

	const readers = 12
	const readsPerReader = 10
	errCh := make(chan error, readers*readsPerReader)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readsPerReader; j++ {
				result, callErr := a.handleListLaunches(context.Background(), mcp.CallToolRequest{})
				if callErr != nil {
					errCh <- callErr
					continue
				}
				if result.IsError {
					errCh <- fmt.Errorf("tool error: %s", textOf(result))
					continue
				}
				if body := textOf(result); !strings.Contains(body, `"id":"stable-launch"`) {
					errCh <- fmt.Errorf("unexpected launch body: %s", body)
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func newCatalogReadFixture(t *testing.T, generation string) string {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir disposable home: %v", err)
	}
	t.Setenv("HOME", home)
	root := filepath.Join(base, "catalog")
	for _, dir := range []string{"projects", "agents", "providers", "launches"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir catalog %s: %v", dir, err)
		}
	}
	writeFile(t, filepath.Join(root, "global.yaml"), "version: 0.1.0\n")
	writeCatalogGeneration(t, root, generation)
	return root
}

func writeCatalogGeneration(t *testing.T, root, generation string) {
	t.Helper()
	files := map[string]string{
		"projects":  fmt.Sprintf("id: %s-project\nname: %s project\nrepo_root: %s\n", generation, generation, filepath.Join(root, generation+"-repo")),
		"agents":    fmt.Sprintf("id: %s-agent\nname: %s agent\n", generation, generation),
		"providers": fmt.Sprintf("id: %s-provider\ntype: cli-goprovider\nprovider: claude\nruntime_kind: subprocess\nadapter: claude\ncommand: /usr/bin/true\n", generation),
		"launches":  fmt.Sprintf("id: %s-launch\nproject: %s-project\nagent: %s-agent\nprovider: %s-provider\n", generation, generation, generation, generation),
	}
	for dir, contents := range files {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("read catalog %s: %v", dir, err)
		}
		for _, entry := range entries {
			if err := os.Remove(filepath.Join(root, dir, entry.Name())); err != nil {
				t.Fatalf("remove old catalog file: %v", err)
			}
		}
		writeFile(t, filepath.Join(root, dir, generation+".yaml"), contents)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertCatalogReadIDs(t *testing.T, a *Adapter, generation string) {
	t.Helper()
	checks := []struct {
		name    string
		key     string
		wantID  string
		handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
	}{
		{name: "projects", key: "projects", wantID: generation + "-project", handler: a.handleListProjects},
		{name: "agents", key: "agents", wantID: generation + "-agent", handler: a.handleListAgents},
		{name: "providers", key: "providers", wantID: generation + "-provider", handler: a.handleListProviders},
		{name: "launches", key: "launches", wantID: generation + "-launch", handler: a.handleListLaunches},
	}
	for _, check := range checks {
		t.Run(check.name+"_"+generation, func(t *testing.T) {
			result, err := check.handler(context.Background(), mcp.CallToolRequest{})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if result.IsError {
				t.Fatalf("tool error: %s", textOf(result))
			}
			body := parseToolJSON(t, result)
			items, ok := body[check.key].([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("%s = %#v, want one item", check.key, body[check.key])
			}
			item, ok := items[0].(map[string]any)
			if !ok || item["id"] != check.wantID {
				t.Fatalf("%s[0] = %#v, want id %q", check.key, items[0], check.wantID)
			}
			assertCatalogObservation(t, body)
		})
	}
}

func assertCatalogObservation(t *testing.T, body map[string]any) {
	t.Helper()
	read, ok := body["catalog_read"].(map[string]any)
	if !ok {
		t.Fatalf("catalog_read = %#v, want object", body["catalog_read"])
	}
	if got := read["status"]; got != "current" {
		t.Fatalf("catalog status = %v, want current", got)
	}
	if got := read["source"]; got != "catalog_root" {
		t.Fatalf("catalog source = %v, want catalog_root", got)
	}
	if got := read["validated"]; got != true {
		t.Fatalf("catalog validated = %v, want true", got)
	}
	if read["observed_at"] == "" {
		t.Fatal("catalog read omitted observed_at")
	}
}
