package mcpadapter

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"slices"
	"testing"
)

func TestProfileOriginBoundaryFlatAndSearch(t *testing.T) {
	for _, mode := range []string{"flat", "search"} {
		for _, tc := range []struct {
			name             string
			servers          []string
			native, upstream bool
		}{
			{"upstream", []string{"alpha"}, false, true},
			{"both", []string{"alpha", "tether"}, true, true},
			{"none", []string{}, false, false},
			{"native", []string{"tether"}, true, false},
			{"omitted", nil, true, true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				raw, _ := json.Marshal(mcpgateway.Profile{Servers: tc.servers})
				t.Setenv("TETHER_PROXY_HELPER_PROFILE_JSON", string(raw))
				catalog, fixtures := proxyCatalog(t, "alpha", "beta")
				cs := connectProxyMode(t, catalog, mode, false, "alpha")
				if started(fixtures, "alpha") != tc.upstream || started(fixtures, "beta") {
					t.Fatal("profile started an unselected origin")
				}
				names := listToolNames(t, cs)
				if mode == "flat" {
					if slices.Contains(names, "tether_health") != tc.native || slices.Contains(names, "alpha_probe") != tc.upstream {
						t.Fatalf("surface=%v", names)
					}
				}
				for _, target := range []struct {
					name     string
					eligible bool
				}{{"tether_health", tc.native}, {"alpha_probe", tc.upstream}} {
					name := target.name
					args := map[string]any{}
					if mode == "search" {
						hydrate, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_tool_list", Arguments: map[string]any{"names": []string{target.name}}})
						if err != nil || hydrate.IsError {
							t.Fatalf("hydrate=%v %v", hydrate, err)
						}
						item := parseToolJSON(t, hydrate)["items"].([]any)[0].(map[string]any)
						if (item["error"] == nil) != target.eligible {
							t.Fatalf("hydration=%v", item)
						}
						args = map[string]any{"name": target.name, "arguments": args}
						name = "tether_tool_call"
					}
					result, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
					if target.eligible {
						if err != nil || result.IsError {
							t.Fatalf("eligible call=%v %v", result, err)
						}
					} else if err == nil && !result.IsError {
						t.Fatal("excluded target dispatched")
					}
				}
			})
		}
	}
}

func TestProfileWireOrderHintsAndDeniedPins(t *testing.T) {
	for _, mode := range []string{"flat", "search"} {
		t.Run(mode, func(t *testing.T) {
			profile := mcpgateway.Profile{Servers: []string{"beta", "alpha", "tether"}, Tools: mcpgateway.ToolRules{Allow: []string{"alpha_probe", "beta_probe", "tether_health"}, Deny: []string{"tether_health"}}, Order: []string{"tether_health", "alpha_probe"}, AlwaysLoad: []string{"tether_health", "alpha_probe"}, Instructions: "Use eligible tools only."}
			raw, _ := json.Marshal(profile)
			t.Setenv("TETHER_PROXY_HELPER_PROFILE_JSON", string(raw))
			catalog, _ := proxyCatalog(t, "alpha", "beta")
			cs := connectProxyMode(t, catalog, mode, false, "alpha", "beta")
			if got := cs.InitializeResult().Instructions; got != profile.Instructions {
				t.Fatalf("instructions=%q", got)
			}
			if mode == "flat" {
				tools, err := cs.ListTools(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(tools.Tools) != 3 || tools.Tools[0].Name != "alpha_probe" || tools.Tools[1].Name != "beta_probe" {
					t.Fatalf("wire order=%v", listToolNames(t, cs))
				}
				if tools.Tools[0].Meta["anthropic/alwaysLoad"] != true {
					t.Fatal("eligible load hint missing")
				}
			} else {
				result, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_tool_list", Arguments: map[string]any{}})
				if err != nil || result.IsError {
					t.Fatalf("list=%v %v", result, err)
				}
				items := parseToolJSON(t, result)["items"].([]any)
				if len(items) != 2 || items[0].(map[string]any)["name"] != "alpha_probe" || items[1].(map[string]any)["name"] != "beta_probe" {
					t.Fatalf("enumeration=%v", items)
				}
				if items[0].(map[string]any)["_meta"].(map[string]any)["anthropic/alwaysLoad"] != true {
					t.Fatal("hydrated load hint missing")
				}
			}
			if _, isErr := tetherCallIfSearch(t, cs, mode, "tether_health"); !isErr {
				t.Fatal("excluded pin restored target")
			}
		})
	}
}
func tetherCallIfSearch(t *testing.T, cs *mcpsdk.ClientSession, mode, name string) (string, bool) {
	t.Helper()
	if mode == "search" {
		return tetherCall(t, cs, name)
	}
	result, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: map[string]any{}})
	if err != nil {
		return err.Error(), true
	}
	return textOf(result), result.IsError
}

func TestProfileReadOnlyAppliesBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"flat", "search"} {
		t.Run(mode, func(t *testing.T) {
			raw, _ := json.Marshal(mcpgateway.Profile{Servers: []string{"tether", "alpha"}, ReadOnly: true, Tools: mcpgateway.ToolRules{Allow: []string{"tether_health", "tether_agent_create", "alpha_probe"}}})
			t.Setenv("TETHER_PROXY_HELPER_PROFILE_JSON", string(raw))
			catalog, _ := proxyCatalog(t, "alpha")
			cs := connectProxyMode(t, catalog, mode, false, "alpha")
			if mode == "flat" {
				names := listToolNames(t, cs)
				if slices.Contains(names, "tether_agent_create") || slices.Contains(names, "alpha_probe") || !slices.Contains(names, "tether_health") {
					t.Fatalf("read-only=%v", names)
				}
			}
			for _, target := range []string{"tether_agent_create", "alpha_probe"} {
				if _, isErr := tetherCallIfSearch(t, cs, mode, target); !isErr {
					t.Fatalf("read-only dispatched %s", target)
				}
				if mode == "search" {
					res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_tool_list", Arguments: map[string]any{"names": []string{target}}})
					if err != nil || res.IsError {
						t.Fatalf("hydrate=%v %v", res, err)
					}
					if parseToolJSON(t, res)["items"].([]any)[0].(map[string]any)["error"] == nil {
						t.Fatal("read-only leaked schema")
					}
				}
			}
		})
	}
}

func TestProfileStartupRejectsInvalidReferences(t *testing.T) {
	catalog, fixtures := proxyCatalog(t, "alpha")
	for _, profile := range []mcpgateway.Profile{{Servers: []string{"bogus"}}, {Servers: []string{"tether"}, Order: []string{"missing_pin"}}, {Servers: []string{"tether"}, AlwaysLoad: []string{"missing_load"}}} {
		for _, proxy := range []bool{false, true} {
			adapter := newTestAdapter(t)
			err := adapter.RunWithGatewayOpts(context.Background(), catalog, ProxyOptions{Profile: mcpgateway.ProfileSelection{ID: "bad", Profile: &profile}}, proxy)
			if err == nil {
				t.Fatalf("invalid profile accepted: %+v", profile)
			}
		}
	}
	if started(fixtures, "alpha") {
		t.Fatal("invalid profile started unselected upstream")
	}
}

func TestProfileCannotWidenConfinedEmptyGrant(t *testing.T) {
	raw, _ := json.Marshal(mcpgateway.Profile{Servers: []string{"alpha", "tether"}})
	t.Setenv("TETHER_PROXY_HELPER_PROFILE_JSON", string(raw))
	catalog, fixtures := proxyCatalog(t, "alpha")
	cs := connectProxyMode(t, catalog, "search", true)
	if started(fixtures, "alpha") {
		t.Fatal("profile widened an empty launch upstream grant")
	}
	if _, isErr := tetherCall(t, cs, "alpha_probe"); !isErr {
		t.Fatal("profile bypassed confinement")
	}
}
