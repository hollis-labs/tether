package mcpadapter

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func rewriteProfileFixture(t *testing.T, catalog, id string, edit func(*config.MCPServerEntry)) {
	t.Helper()
	path := filepath.Join(catalog, "mcp-servers", id+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entry config.MCPServerEntry
	if err := yaml.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	edit(&entry)
	raw, err = yaml.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProfileSlashToolDeniedOnRealProxy(t *testing.T) {
	for _, mode := range []string{"flat", "search"} {
		t.Run(mode, func(t *testing.T) {
			catalog, fixtures := proxyCatalog(t, "alpha")
			rewriteProfileFixture(t, catalog, "alpha", func(entry *config.MCPServerEntry) { entry.Env["TETHER_UPSTREAM_TOOL_NAME"] = "ns/delete_all" })
			raw, _ := json.Marshal(mcpgateway.Profile{Tools: mcpgateway.ToolRules{Allow: []string{"*"}, Deny: []string{"*delete*"}}})
			t.Setenv("TETHER_PROXY_HELPER_PROFILE_JSON", string(raw))
			cs := connectProxyMode(t, catalog, mode, false, "alpha")
			if slices.Contains(listToolNames(t, cs), "ns/delete_all") {
				t.Fatal("slash deny leaked schema")
			}
			if _, isErr := tetherCallIfSearch(t, cs, mode, "ns/delete_all"); !isErr {
				t.Fatal("slash deny dispatched")
			}
			if mode == "search" {
				for _, args := range []map[string]any{{"names": []string{"ns/delete_all"}}, {"servers": []string{"alpha"}}} {
					result, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_tool_list", Arguments: args})
					if args["servers"] != nil {
						if err == nil && !result.IsError {
							t.Fatal("fully excluded server accepted")
						}
					} else if err != nil || parseToolJSON(t, result)["items"].([]any)[0].(map[string]any)["error"] == nil {
						t.Fatalf("slash hydration=%v %v", result, err)
					}
				}
			}
			events, _ := os.ReadFile(filepath.Join(fixtures, "alpha.events"))
			if strings.Contains(string(events), "call ") {
				t.Fatal("excluded slash name reached upstream")
			}
		})
	}
}

func TestProfileUnavailablePinsStartDegraded(t *testing.T) {
	catalog, _ := proxyCatalog(t, "alpha")
	rewriteProfileFixture(t, catalog, "alpha", func(entry *config.MCPServerEntry) { entry.Command = "/missing-profile-upstream" })
	raw, _ := json.Marshal(mcpgateway.Profile{Servers: []string{"alpha"}, Order: []string{"alpha_probe"}, AlwaysLoad: []string{"alpha_probe"}})
	t.Setenv("TETHER_PROXY_HELPER_PROFILE_JSON", string(raw))
	cs := connectProxyMode(t, catalog, "flat", false, "alpha")
	result, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_gateway_status", Arguments: map[string]any{}})
	if err != nil || result.IsError || !strings.Contains(textOf(result), "upstream alpha unavailable") {
		t.Fatalf("degraded status=%v %v", result, err)
	}
	if slices.Contains(listToolNames(t, cs), "alpha_probe") {
		t.Fatal("unavailable pin exposed")
	}
}

func TestNoProfileLegacyTetherUpstreamAndUnknownCallShape(t *testing.T) {
	catalog, _ := proxyCatalog(t, "tether")
	cs := connectProxyMode(t, catalog, "flat", false)
	if !slices.Contains(listToolNames(t, cs), "tether_probe") {
		t.Fatal("legacy tether upstream lost")
	}
	if _, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "unknown_tool", Arguments: map[string]any{}}); err == nil {
		t.Fatal("unknown direct name changed from JSON-RPC error to tool result")
	}
	if result, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_probe", Arguments: map[string]any{}}); err != nil || result.IsError {
		t.Fatalf("legacy upstream call=%v %v", result, err)
	}
	profile := mcpgateway.Profile{Servers: []string{"tether"}}
	if err := newTestAdapter(t).RunWithGatewayOpts(context.Background(), catalog, ProxyOptions{Profile: mcpgateway.ProfileSelection{Profile: &profile}}, true); err == nil {
		t.Fatal("selected reserved origin collision accepted")
	}
}

func TestProfileRestrictedOriginStatusHint(t *testing.T) {
	catalog, _ := proxyCatalog(t, "alpha", "beta")
	raw, _ := json.Marshal(mcpgateway.Profile{Servers: []string{"beta"}})
	t.Setenv("TETHER_PROXY_HELPER_PROFILE_JSON", string(raw))
	cs := connectProxyMode(t, catalog, "search", true, "alpha")
	result, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_gateway_status", Arguments: map[string]any{}})
	if err != nil || !strings.Contains(textOf(result), "profile origin beta excluded by upstream restriction or confined grant") {
		t.Fatalf("restricted hint=%v %v", result, err)
	}
}

func TestProfileRefResolverUsesEligibility(t *testing.T) {
	called := false
	policy := &mcpgateway.Policy{Selection: mcpgateway.ProfileSelection{Profile: &mcpgateway.Profile{Tools: mcpgateway.ToolRules{Deny: []string{"tesseract_*"}}}}}
	service := &mcpgateway.Service{Policy: policy, Snapshot: func() mcpgateway.Snapshot {
		return mcpgateway.Snapshot{Entries: []mcpgateway.Entry{{Tool: &mcpsdk.Tool{Name: "tesseract_ref_resolve"}, Origin: "tesseract"}}}
	}, Dispatch: func(context.Context, string, map[string]any, map[string]any) (*mcpsdk.CallToolResult, error) {
		called = true
		return nil, nil
	}}
	resolver := &routerRefResolver{router: NewProxyRouter(NewToolRegistry()), gateway: service}
	if _, err := resolver.ResolveRef(context.Background(), map[string]any{}); err == nil || !strings.Contains(err.Error(), "excluded") {
		t.Fatalf("ref eligibility=%v", err)
	}
	if called {
		t.Fatal("excluded resolver dispatched")
	}
}
