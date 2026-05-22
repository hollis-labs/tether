package mcpadapter

import (
	"sort"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func proxySurfaceToolNamesForTest(t *testing.T, serverFilter []string, only bool) []string {
	t.Helper()

	reg := NewToolRegistry()
	mc := &mockClient{}
	reg.Register("alpha", mc, []mcp.Tool{makeTool("alpha_tool_a"), makeTool("alpha_tool_b")})
	reg.Register("beta", mc, []mcp.Tool{makeTool("beta_tool_x")})

	s := server.NewMCPServer("test", version, server.WithToolCapabilities(true))
	a := &Adapter{}
	if !only {
		a.registerTools(s)
	}

	allowed := make(map[string]struct{}, len(serverFilter))
	for _, id := range serverFilter {
		allowed[id] = struct{}{}
	}
	firehose := len(allowed) == 0
	router := NewProxyRouter(reg)
	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)
	live := &liveProxyCatalog{
		adapter:  a,
		server:   s,
		registry: reg,
		router:   router,
		index:    idx,
		allowed:  allowed,
		firehose: firehose,
	}

	var proxied []mcp.Tool
	for _, def := range reg.AllDefinitions() {
		rt, ok := reg.Lookup(def.Name)
		if !ok || rt.ServerID == "" {
			continue
		}
		if !firehose {
			if _, ok := allowed[rt.ServerID]; !ok {
				continue
			}
		}
		proxied = append(proxied, def)
	}
	live.addProxyTools(proxied...)

	if !only {
		a.registerDiscoverTool(s, idx, allowed, firehose)
		a.registerSemanticDiscoverTool(s, idx, allowed, firehose)
		a.registerCallTool(s, router)
		a.registerMCPServersTool(s, NewClientPool(nil, reg), nil, allowed, firehose)
		a.registerCatalogRefreshTool(s, NewClientPool(nil, reg))
	}

	names := make([]string, 0, len(s.ListTools()))
	for name := range s.ListTools() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func hasToolName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func TestProxySurfaceDefaultKeepsNativeAndHatchTools(t *testing.T) {
	names := proxySurfaceToolNamesForTest(t, nil, false)
	for _, want := range []string{
		"alpha_tool_a",
		"alpha_tool_b",
		"beta_tool_x",
		"mux_health",
		"mux_discover",
		"mux_discover_tools",
		"mux_call",
		"mux_catalog_list_mcp_servers",
	} {
		if !hasToolName(names, want) {
			t.Fatalf("default proxy missing %q in %v", want, names)
		}
	}
}

func TestProxySurfaceServersKeepsNativeAndHatchTools(t *testing.T) {
	names := proxySurfaceToolNamesForTest(t, []string{"alpha"}, false)
	for _, want := range []string{
		"alpha_tool_a",
		"alpha_tool_b",
		"mux_health",
		"mux_discover",
		"mux_discover_tools",
		"mux_call",
		"mux_catalog_list_mcp_servers",
	} {
		if !hasToolName(names, want) {
			t.Fatalf("selective proxy missing %q in %v", want, names)
		}
	}
	if hasToolName(names, "beta_tool_x") {
		t.Fatalf("selective proxy included beta tool: %v", names)
	}
}

func TestProxySurfaceOnlySuppressesMuxTools(t *testing.T) {
	names := proxySurfaceToolNamesForTest(t, []string{"alpha"}, true)
	want := []string{"alpha_tool_a", "alpha_tool_b"}
	if len(names) != len(want) {
		t.Fatalf("only proxy tools = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("only proxy tools = %v, want %v", names, want)
		}
	}
	for _, name := range names {
		if len(name) >= 4 && name[:4] == "mux_" {
			t.Fatalf("only proxy exposed mux tool %q in %v", name, names)
		}
	}
}
