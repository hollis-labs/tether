package mcpadapter

import (
	"context"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestProxySurfaceFlatHasNoDiscoveryHatch(t *testing.T) {
	catalog, _ := proxyCatalog(t, "alpha", "beta")
	cs := connectProxy(t, catalog, false, "alpha")
	names := listToolNames(t, cs)
	for _, name := range []string{"alpha_probe", "tether_health", "tether_gateway_status"} {
		if !slices.Contains(names, name) {
			t.Fatalf("missing %s: %v", name, names)
		}
	}
	for _, name := range []string{"beta_probe", "tether_tool_search", "tether_tool_list", "tether_tool_call"} {
		if slices.Contains(names, name) {
			t.Fatalf("unexpected %s: %v", name, names)
		}
	}
}
func TestProxySurfaceSearchHasOnlyFixedFront(t *testing.T) {
	catalog, _ := proxyCatalog(t, "alpha", "beta")
	cs := connectProxyMode(t, catalog, "search", false, "alpha")
	names := listToolNames(t, cs)
	want := []string{"tether_gateway_status", "tether_tool_call", "tether_tool_list", "tether_tool_search"}
	if !slices.Equal(names, want) {
		t.Fatalf("search front = %v, want %v", names, want)
	}
}

func TestSearchModeHydratesAndCallsNativeTargets(t *testing.T) {
	catalog, _ := proxyCatalog(t, "alpha")
	cs := connectProxyMode(t, catalog, "search", true, "alpha")
	result, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_tool_list", Arguments: map[string]any{"names": []string{"tether_health", "alpha_probe", "beta_probe"}}})
	if err != nil || result.IsError {
		t.Fatalf("hydrate: %v %v", result, err)
	}
	body := parseToolJSON(t, result)
	items := body["items"].([]any)
	for _, i := range []int{0, 1} {
		if items[i].(map[string]any)["inputSchema"] == nil {
			t.Fatalf("missing real schema: %+v", items[i])
		}
	}
	if items[2].(map[string]any)["error"] == nil {
		t.Fatal("excluded target hydrated")
	}
	result, err = cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_tool_call", Arguments: map[string]any{"name": "tether_health", "arguments": map[string]any{}}})
	if err != nil || result.IsError {
		t.Fatalf("native dispatch: %v %v", result, err)
	}
	if parseToolJSON(t, result)["ok"] != true {
		t.Fatal("native result not preserved")
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "tether_tool_call" && (tool.Annotations == nil || tool.Annotations.ReadOnlyHint) {
			t.Fatal("dispatcher claimed read-only")
		}
	}
}

func TestGatewayStartupRejectsInvalidSelections(t *testing.T) {
	catalog, fixtures := proxyCatalog(t, "alpha")
	for _, opts := range []ProxyOptions{
		{ServerFilter: []string{"not-configured"}},
		{ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: "flat", Source: "argument"}}, Gateway: stringPointer("directory")}},
	} {
		if err := newTestAdapter(t).RunWithProxyOpts(context.Background(), catalog, opts); err == nil {
			t.Fatal("invalid selection accepted")
		}
		if started(fixtures, "alpha") {
			t.Fatal("invalid selection started an upstream")
		}
	}
	disabled := false
	entry := fixtureEntry(t, fixtures, "disabled")
	entry.Enabled = &disabled
	raw, err := yaml.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalog, "mcp-servers", "disabled.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newTestAdapter(t).RunWithProxyOpts(context.Background(), catalog, ProxyOptions{ServerFilter: []string{"disabled"}}); err == nil {
		t.Fatal("disabled origin accepted")
	}
}
func stringPointer(value string) *string { return &value }
