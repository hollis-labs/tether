package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNamingPrefixedExtractionUsesOriginalName(t *testing.T) {
	for _, mode := range []mcpgateway.Mode{mcpgateway.Flat, mcpgateway.Search} {
		for _, tc := range []struct {
			name, response string
			count          int
		}{
			{"workspace_write", `{"status":"replayed","item_id":"01M2ITEM000000000000000000"}`, 0},
			{"tesseract_recall", `{"item_id":"01M2ITEM000000000000000000"}`, 0},
			{"tesseract_ref_resolve", `{"item_id":"01M2ITEM000000000000000000"}`, 0},
			{"workspace_write", `{"status":"created","item_id":"01M2ITEM000000000000000000"}`, 1},
		} {
			t.Run(string(mode)+tc.name+tc.response, func(t *testing.T) {
				a, refs := newExtractingAdapter("session", true)
				r := NewToolRegistry()
				r.SetPrefix("tesseract", "ts_")
				client := &mockClient{callToolFunc: func(_ context.Context, p *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
					if p.Name != tc.name {
						t.Fatalf("upstream name %q", p.Name)
					}
					return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: tc.response}}}, nil
				}}
				mustRegister(t, r, "tesseract", client, []*mcpsdk.Tool{makeTool(tc.name)})
				s := gomcp.NewServer("test", "1")
				router := NewProxyRouter(r)
				name, args := "ts_"+tc.name, map[string]any{}
				if mode == mcpgateway.Flat {
					live := &liveProxyCatalog{adapter: a, server: s, registry: r, router: router, firehose: true}
					live.addProxyTools(r.AllDefinitions()...)
				} else {
					a.registerCallTool(s, a.gatewayService(r, router, mcpgateway.Selection{Mode: mode}, nil))
					args = map[string]any{"name": name, "arguments": args}
					name = "tether_tool_call"
				}
				res, err := connectInMemory(t, s).CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
				if err != nil || res.IsError || len(refs.got) != tc.count {
					t.Fatalf("call %v %v refs %+v", res, err, refs.got)
				}
			})
		}
	}
}

func TestNamingPrefixedResolverUsesFinalPolicyAndOriginalDispatch(t *testing.T) {
	a := newTestAdapter(t)
	r := NewToolRegistry()
	r.SetPrefix("tesseract", "ts_")
	calls := 0
	client := &mockClient{callToolFunc: func(_ context.Context, p *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		calls++
		if p.Name != "tesseract_ref_resolve" {
			t.Fatal(p.Name)
		}
		return &mcpsdk.CallToolResult{StructuredContent: map[string]any{"status": "resolved"}}, nil
	}}
	mustRegister(t, r, "tesseract", client, []*mcpsdk.Tool{makeTool("tesseract_ref_resolve")})
	router := NewProxyRouter(r)
	g := a.gatewayService(r, router, mcpgateway.Selection{Mode: mcpgateway.Search}, nil)
	resolver := &routerRefResolver{router: router, gateway: g}
	if _, err := resolver.ResolveRef(context.Background(), map[string]any{"key": "test"}); err != nil {
		t.Fatal(err)
	}
	r.SetPrefix("excluded", "other_")
	mustRegister(t, r, "excluded", client, []*mcpsdk.Tool{makeTool("tesseract_ref_resolve")})
	g.Policy = &mcpgateway.Policy{Selection: mcpgateway.ProfileSelection{Profile: &mcpgateway.Profile{Tools: mcpgateway.ToolRules{Deny: []string{"other_*"}}}}}
	if _, err := resolver.ResolveRef(context.Background(), nil); err != nil || calls != 2 {
		t.Fatalf("excluded resolver caused ambiguity: %v calls %d", err, calls)
	}
	g.Policy.Selection.Profile.Tools.Deny = []string{"ts_*", "other_*"}
	if _, err := resolver.ResolveRef(context.Background(), nil); err == nil || calls != 2 {
		t.Fatalf("denied resolution %v calls %d", err, calls)
	}
}

func TestNamingUnselectedReservedOriginDoesNotBreakProxy(t *testing.T) {
	for _, mode := range []string{"flat", "search"} {
		t.Run(mode+"only", func(t *testing.T) {
			t.Setenv("TETHER_PROXY_HELPER_ONLY", "1")
			catalog, fixtures := proxyCatalog(t, "alpha", "tether")
			cs := connectProxyMode(t, catalog, mode, false, "alpha")
			if _, failed := tetherCallIfSearch(t, cs, mode, "alpha_probe"); failed || started(fixtures, "tether") {
				t.Fatal("unselected reserved upstream affected only mode")
			}
		})
		for _, confine := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "confined", false: "selected"}[confine], func(t *testing.T) {
				catalog, fixtures := proxyCatalog(t, "alpha", "tether")
				cs := connectProxyMode(t, catalog, mode, confine, "alpha")
				if _, failed := tetherCallIfSearch(t, cs, mode, "alpha_probe"); failed || started(fixtures, "tether") {
					t.Fatal("unselected reserved upstream affected proxy")
				}
			})
		}
		t.Run(mode+"profile", func(t *testing.T) {
			catalog, fixtures := proxyCatalog(t, "alpha", "tether")
			raw, _ := json.Marshal(mcpgateway.Profile{Servers: []string{"alpha"}})
			t.Setenv("TETHER_PROXY_HELPER_PROFILE_JSON", string(raw))
			cs := connectProxyMode(t, catalog, mode, false, "alpha", "tether")
			if _, failed := tetherCallIfSearch(t, cs, mode, "alpha_probe"); failed || started(fixtures, "tether") {
				t.Fatal("profile-excluded reserved origin affected proxy")
			}
		})
	}
}

func TestNamingPoolCollisionPreservesDiagnosticInventory(t *testing.T) {
	r := NewToolRegistry()
	p := NewClientPool([]config.MCPServerEntry{{ID: "alpha"}, {ID: "beta"}, {ID: "gamma"}}, r)
	p.SetConnectFunc(func(_ context.Context, e config.MCPServerEntry) (upstreamClient, error) {
		name := "read"
		if e.ID == "gamma" {
			name = "gamma_read"
		}
		return &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool(name)}}, nil
		}}, nil
	})
	defer p.Shutdown()
	var collision *mcpgateway.CollisionError
	if err := p.Start(context.Background()); !errors.As(err, &collision) {
		t.Fatal(err)
	}
	statuses := p.StatusSummary()
	if len(statuses) != 3 {
		t.Fatalf("lost statuses %+v", statuses)
	}
	for _, s := range statuses {
		if s.ID == "gamma" && (s.Status != "connected" || s.Degraded) {
			t.Fatalf("healthy server affected %+v", s)
		}
	}
	if _, ok := r.Lookup("gamma_read"); !ok {
		t.Fatal("healthy inventory lost")
	}
}

func TestNamingCollisionClearsWhenOpposingOwnerDropsName(t *testing.T) {
	r := NewToolRegistry()
	mustRegister(t, r, "alpha", nil, []*mcpsdk.Tool{makeTool("alpha_read")})
	mustRegister(t, r, "beta", nil, []*mcpsdk.Tool{makeTool("beta_read")})
	if _, err := r.ReplaceServer("alpha", nil, []*mcpsdk.Tool{makeTool("beta_read")}); err == nil {
		t.Fatal("expected collision")
	}
	if _, err := r.ReplaceServer("beta", nil, []*mcpsdk.Tool{makeTool("beta_new")}); err != nil {
		t.Fatal(err)
	}
	if _, collisions := r.NameDiagnostics(); len(collisions) != 0 {
		t.Fatal(collisions)
	}
	if _, ok := r.Lookup("alpha_read"); !ok {
		t.Fatal("accepted inventory lost")
	}
}

func TestNamingProbeCancellationKillsDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived")
	ctx, cancel := context.WithCancel(context.Background())
	u, _, err := spawnStdioUpstream(config.MCPServerEntry{Command: "sh", Args: []string{"-c", `(sleep 0.5; echo survived > "$1") & wait`, "probe", marker}}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	u.abandon()
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("probe descendant survived cancellation")
	}
}

func TestNamingProbeRedactsBareHostname(t *testing.T) {
	s := probeRedactor([]config.MCPServerEntry{{URL: "https://private.example.invalid/path"}})
	if got := redactProbeText(s, "dial tcp private.example.invalid:443: failed"); strings.Contains(got, "private.example.invalid") {
		t.Fatal(got)
	}
}
