package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNamingRaceFailsWithoutAliases(t *testing.T) {
	for n := 0; n < 20; n++ {
		registry := NewToolRegistry()
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, id := range []string{"alpha", "beta"} {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				<-start
				results <- registry.Register(id, nil, []*mcpsdk.Tool{makeTool("read")})
			}(id)
		}
		close(start)
		wg.Wait()
		close(results)
		failed := 0
		for err := range results {
			if err != nil {
				failed++
				if !strings.Contains(err.Error(), `origin "alpha"`) || !strings.Contains(err.Error(), `origin "beta"`) || !strings.Contains(err.Error(), `final tool name "read"`) {
					t.Fatal(err)
				}
			}
		}
		if failed != 1 || len(registry.AllDefinitions()) != 1 {
			t.Fatal("collision did not reject one complete registration")
		}
		for _, alias := range []string{"alpha__read", "beta__read"} {
			if _, ok := registry.Lookup(alias); ok {
				t.Fatal("alias created")
			}
		}
	}
}
func TestNamingPrefixForwardsOriginalNameAndMetadata(t *testing.T) {
	registry := NewToolRegistry()
	registry.SetPrefix("alpha", "alpha_")
	tool := makeTool("read")
	tool.Title = "Authored title"
	tool.Meta = mcpsdk.Meta{"owner": "alpha"}
	tool.Annotations = &mcpsdk.ToolAnnotations{ReadOnlyHint: true}
	calls := 0
	client := &mockClient{callToolFunc: func(_ context.Context, p *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		calls++
		if p.Name != "read" {
			t.Fatalf("forwarded name=%q", p.Name)
		}
		return &mcpsdk.CallToolResult{}, nil
	}}
	mustRegister(t, registry, "alpha", client, []*mcpsdk.Tool{tool})
	rt, ok := registry.Lookup("alpha_read")
	if !ok || rt.UpstreamName != "read" || rt.Definition.Title != tool.Title || rt.Definition.Meta["owner"] != "alpha" || rt.Definition.Annotations != tool.Annotations || tool.Name != "read" {
		t.Fatal("prefix mutated metadata or original name")
	}
	if _, err := NewProxyRouter(registry).Handle(context.Background(), ToolCall{ToolName: "alpha_read"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("target not forwarded")
	}
}
func TestNamingGatewayReservedAndAtomicRefresh(t *testing.T) {
	for _, name := range []string{"tether_gateway_status", "tether_tool_search", "tether_tool_list", "tether_tool_call"} {
		r := NewToolRegistry()
		if err := r.Register("alpha", nil, []*mcpsdk.Tool{makeTool(name)}); err == nil {
			t.Fatalf("reserved %s accepted", name)
		}
	}
	r := NewToolRegistry()
	mustRegister(t, r, "alpha", nil, []*mcpsdk.Tool{makeTool("alpha_read")})
	mustRegister(t, r, "beta", nil, []*mcpsdk.Tool{makeTool("beta_read")})
	if _, err := r.ReplaceServer("alpha", nil, []*mcpsdk.Tool{makeTool("beta_read"), makeTool("alpha_new")}); err == nil {
		t.Fatal("refresh collision accepted")
	}
	if _, ok := r.Lookup("alpha_read"); !ok {
		t.Fatal("refresh dropped last accepted schema")
	}
	if _, ok := r.Lookup("alpha_new"); ok {
		t.Fatal("refresh partially published")
	}
	_, collisions := r.NameDiagnostics()
	if len(collisions) != 1 {
		t.Fatal("status finding missing")
	}
}
func TestNamingPoolStartupCollisionIsHardError(t *testing.T) {
	r := NewToolRegistry()
	p := NewClientPool([]config.MCPServerEntry{{ID: "alpha", Transport: "http"}, {ID: "beta", Transport: "http"}}, r)
	p.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
		return &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool("read")}}, nil
		}}, nil
	})
	defer p.Shutdown()
	err := p.Start(context.Background())
	var collision *mcpgateway.CollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("startup collision=%v", err)
	}
}
func TestNamingPaginationChecksLateCollision(t *testing.T) {
	calls := 0
	client := &mockClient{listToolsFunc: func(_ context.Context, p *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
		calls++
		if p.Cursor == "" {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool("alpha_read")}, NextCursor: "late"}, nil
		}
		return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool("tether_tool_call")}}, nil
	}}
	all, err := listUpstreamTools(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewToolRegistry().Register("alpha", client, all.Tools); err == nil || calls != 2 {
		t.Fatal("late reserved collision ignored")
	}
}

func TestNamingReconnectCollisionKeepsAcceptedRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := NewToolRegistry()
	pool := NewClientPool([]config.MCPServerEntry{fixtureEntry(t, dir, "alpha"), fixtureEntry(t, dir, "beta")}, registry)
	pool.policy.Delays = []time.Duration{10 * time.Millisecond}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	old, _ := registry.Lookup("alpha_probe")
	if err := os.WriteFile(filepath.Join(dir, "alpha.tool"), []byte("beta_probe"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureMarker(t, dir, "alpha", "1")
	awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "connected" && s.Degraded })
	rt, ok := registry.Lookup("alpha_probe")
	if !ok || rt.Client == old.Client {
		t.Fatal("accepted schema lost or failed to rebind live connection")
	}
	if def, _ := registry.Lookup("beta_probe"); def.ServerID != "beta" {
		t.Fatal("restart replaced sibling")
	}
	a := newTestAdapter(t)
	a.upstreams = pool
	service := a.gatewayService(registry, NewProxyRouter(registry), mcpgateway.Selection{Mode: mcpgateway.Flat}, nil)
	status := service.Status("")
	if len(status.Collisions) != 1 || status.Complete {
		t.Fatalf("degraded status=%+v", status)
	}
	tools, _, err := service.ProtocolList(registry.AllDefinitions(), isGatewayTool, "")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "alpha_probe") {
		t.Fatal("rejected restart dropped visible accepted schema")
	}
}
func TestNamingRealProxyPrefixAndPolicyUseFinalName(t *testing.T) {
	for _, mode := range []string{"flat", "search"} {
		t.Run(mode, func(t *testing.T) {
			catalog, _ := proxyCatalog(t, "alpha")
			rewriteProfileFixture(t, catalog, "alpha", func(e *config.MCPServerEntry) {
				e.ToolPrefix = "alpha_"
				e.Env["TETHER_UPSTREAM_TOOL_NAME"] = "read"
				e.Env["TETHER_UPSTREAM_EXPECT_NAME"] = "read"
			})
			raw, _ := json.Marshal(mcpgateway.Profile{Tools: mcpgateway.ToolRules{Allow: []string{"alpha_read"}}, Order: []string{"alpha_read"}})
			t.Setenv("TETHER_PROXY_HELPER_PROFILE_JSON", string(raw))
			cs := connectProxyMode(t, catalog, mode, false, "alpha")
			if _, isErr := tetherCallIfSearch(t, cs, mode, "alpha_read"); isErr {
				t.Fatal("final name did not forward original")
			}
			if _, isErr := tetherCallIfSearch(t, cs, mode, "read"); !isErr {
				t.Fatal("unprefixed name remains callable")
			}
		})
	}
}
func TestNamingLiveProbeCollisionParityNoToolCallsAndTimeout(t *testing.T) {
	catalog, fixtures := proxyCatalog(t, "alpha", "beta")
	for _, id := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(fixtures, id+".tool"), []byte("read"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	adapter := newTestAdapter(t)
	status, err := adapter.ProbeNames(context.Background(), catalog, ProxyOptions{})
	if err == nil || len(status.Collisions) != 1 {
		t.Fatalf("probe collision=%+v %v", status, err)
	}
	startup := newTestAdapter(t).RunWithGatewayOpts(context.Background(), catalog, ProxyOptions{}, true)
	if startup == nil || startup.Error() != err.Error() {
		t.Fatalf("startup/probe mismatch %v vs %v", startup, err)
	}
	for _, id := range []string{"alpha", "beta"} {
		events, _ := os.ReadFile(filepath.Join(fixtures, id+".events"))
		if strings.Contains(string(events), "call ") {
			t.Fatal("live probe dispatched a tool")
		}
	}
	hung, dir := proxyCatalog(t, "hung")
	rewriteProfileFixture(t, hung, "hung", func(e *config.MCPServerEntry) { e.Env["TETHER_UPSTREAM_HANG"] = "1" })
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err = newTestAdapter(t).ProbeNames(ctx, hung, ProxyOptions{})
	if err == nil || time.Since(began) > 5*time.Second {
		t.Fatalf("probe did not honor overall deadline: %v %v", time.Since(began), err)
	}
	awaitFixturesExited(t, dir, "hung")
}
func TestNamingLiveProbeUsesConfineAndScrubsTypedJSON(t *testing.T) {
	t.Setenv(config.MCPConfineRemoteEnv, "1")
	catalog, _ := proxyCatalog(t, "alpha")
	rewriteProfileFixture(t, catalog, "alpha", func(e *config.MCPServerEntry) { e.Transport = "http"; e.URL = "http://example.invalid/private-secret" })
	adapter := newTestAdapter(t)
	adapter.SetProtectedPaths([]string{catalog})
	status, err := adapter.ProbeNames(context.Background(), catalog, ProxyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Origins) == 0 {
		t.Fatal("remote exclusion absent")
	}
	excluded := false
	for _, o := range status.Origins {
		if o.ID == "alpha" && o.Status == "excluded" {
			excluded = true
		}
	}
	if !excluded {
		t.Fatalf("remote not excluded: %+v", status)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "example.invalid") {
		t.Fatal("URL leaked in JSON")
	}
	secrets := probeRedactor([]config.MCPServerEntry{{URL: "http://example.invalid/private-secret", Token: "secret-value"}})
	if text := redactProbeText(secrets, "GET http://different.invalid/x?token=secret-value"); strings.Contains(text, "http") || strings.Contains(text, "secret-value") {
		t.Fatal(text)
	}
}
func TestNamingReservedOriginOnlyBreaksGateway(t *testing.T) {
	catalog, _ := proxyCatalog(t, "tether")
	if err := os.WriteFile(filepath.Join(catalog, "global.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(catalog); err != nil {
		t.Fatalf("generic load rejected naming: %v", err)
	}
	err := newTestAdapter(t).RunWithGatewayOpts(context.Background(), catalog, ProxyOptions{}, true)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved startup=%v", err)
	}
}

func TestNamingLiveProbeScrubsInheritedBearer(t *testing.T) {
	t.Setenv("TETHER_TOKEN", "private-probe-caller-token")
	catalog, dir := proxyCatalog(t, "alpha")
	rewriteProfileFixture(t, catalog, "alpha", func(e *config.MCPServerEntry) { e.Env["TETHER_UPSTREAM_RECORD_TOKEN"] = "1" })
	if _, err := newTestAdapter(t).ProbeNames(context.Background(), catalog, ProxyOptions{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "alpha.token"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatal("probe delegated caller bearer")
	}
}
