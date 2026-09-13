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
	assertCatalogFailure(t, read["error"], "decode", "launches")
	if _, exists := healthBody["launches"]; exists {
		t.Fatal("health exposed stale launch counts after catalog reload failure")
	}

	writeCatalogGeneration(t, root, "recovered")
	assertCatalogReadIDs(t, a, "recovered")
}

func TestCatalogReloadErrorsDoNotExposeConfigurationValues(t *testing.T) {
	const marker = "FAKE0015"
	tests := []struct {
		name         string
		global       string
		wantCategory string
		wantLocation string
	}{
		{
			name:         "decoder",
			global:       "version: 0.1.0\nai:\n  providers:\n    - id: test\n      enabled: " + marker + "\n",
			wantCategory: "decode",
			wantLocation: "global",
		},
		{
			name:         "validator",
			global:       "version: 0.1.0\ncatalog:\n  defaults:\n    permission_mode: " + marker + "\n",
			wantCategory: "validation",
			wantLocation: "global.catalog.defaults.permission_mode",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newCatalogReadFixture(t, "good")
			initial, err := config.LoadLayered(root)
			if err != nil {
				t.Fatalf("load initial catalog: %v", err)
			}
			a := New(&app.Service{CatalogRoot: root, Catalog: initial}, "", nil)
			writeFile(t, filepath.Join(root, "global.yaml"), test.global)

			handlers := []struct {
				name   string
				health bool
				call   func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
			}{
				{name: "health", health: true, call: a.handleHealth},
				{name: "projects", call: a.handleListProjects},
				{name: "agents", call: a.handleListAgents},
				{name: "providers", call: a.handleListProviders},
				{name: "launches", call: a.handleListLaunches},
			}
			for _, handler := range handlers {
				result, callErr := handler.call(context.Background(), mcp.CallToolRequest{})
				if callErr != nil {
					t.Fatalf("%s: %v", handler.name, callErr)
				}
				wire := textOf(result)
				if strings.Contains(wire, marker) {
					t.Errorf("%s exposed configuration value: %s", handler.name, wire)
				}
				body := parseToolJSON(t, result)
				if handler.health {
					if result.IsError {
						t.Fatalf("health returned tool error: %s", wire)
					}
					if body["ok"] != false {
						t.Fatalf("health ok = %v, want false", body["ok"])
					}
					if _, exists := body["launches"]; exists {
						t.Fatal("health exposed stale counts after reload failure")
					}
					read := body["catalog_read"].(map[string]any)
					assertCatalogFailure(t, read["error"], test.wantCategory, test.wantLocation)
					continue
				}
				if !result.IsError || body["code"] != "catalog_reload_failed" {
					t.Fatalf("%s result = %#v, want catalog_reload_failed", handler.name, body)
				}
				assertCatalogFailure(t, body["error"], test.wantCategory, test.wantLocation)
			}

			writeFile(t, filepath.Join(root, "global.yaml"), "version: 0.1.0\n")
			result, callErr := a.handleListLaunches(context.Background(), mcp.CallToolRequest{})
			if callErr != nil || result.IsError {
				t.Fatalf("next-read recovery failed: err=%v result=%s", callErr, textOf(result))
			}
		})
	}
}

func TestCatalogReadRejectsIncompleteRecordsAndReferences(t *testing.T) {
	root := newCatalogReadFixture(t, "good")
	initial, err := config.LoadLayered(root)
	if err != nil {
		t.Fatalf("load initial catalog: %v", err)
	}
	a := New(&app.Service{CatalogRoot: root, Catalog: initial}, "", nil)

	writeFile(t, filepath.Join(root, "projects", "good.yaml"), "name: missing id\n")
	result, err := a.handleListProjects(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("missing id read: %v", err)
	}
	if !result.IsError {
		t.Fatalf("missing id result = %s, want tool error", textOf(result))
	}
	body := parseToolJSON(t, result)
	assertCatalogFailure(t, body["error"], "validation", "projects.id")

	writeCatalogGeneration(t, root, "valid")
	writeFile(t, filepath.Join(root, "launches", "valid.yaml"), "id: valid-launch\nproject: valid-project\nagent: valid-agent\nprovider: missing-provider\n")
	result, err = a.handleListLaunches(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("missing reference read: %v", err)
	}
	if !result.IsError {
		t.Fatalf("missing reference result = %s, want tool error", textOf(result))
	}
	body = parseToolJSON(t, result)
	assertCatalogFailure(t, body["error"], "validation", "launches")

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

func assertCatalogFailure(t *testing.T, value any, wantCategory, wantLocation string) {
	t.Helper()
	failure, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("catalog failure = %#v, want object", value)
	}
	if got := failure["category"]; got != wantCategory {
		t.Fatalf("catalog failure category = %v, want %q", got, wantCategory)
	}
	if got := failure["location"]; got != wantLocation {
		t.Fatalf("catalog failure location = %v, want %q", got, wantLocation)
	}
	if len(failure) != 2 {
		t.Fatalf("catalog failure fields = %#v, want bounded category/location", failure)
	}
}
