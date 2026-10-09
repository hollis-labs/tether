//go:build unix

package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// RunWithProxyOpts is where --confine decides which catalog loader runs, and it
// ends by serving on stdio, so nothing short of running it can guard that choice:
// the config loader's own tests would pass if it went back to loading every
// upstream. These tests run it for real. The test binary re-executes itself as
// the proxy (TestRunWithProxyOptsHelper, the same trick as TestUpstreamFixture),
// the parent connects to it as an MCP client over stdio, and the upstreams are the
// real fixture processes, which record a "start" event when they launch
// (CW-20261001-0227 review).

const (
	proxyHelperCatalogEnv = "TETHER_PROXY_HELPER_CATALOG"
	proxyHelperServersEnv = "TETHER_PROXY_HELPER_SERVERS"
	proxyHelperConfineEnv = "TETHER_PROXY_HELPER_CONFINE"
)

// TestRunWithProxyOptsHelper is the proxy process the tests below run. It does
// nothing in an ordinary `go test`.
func TestRunWithProxyOptsHelper(t *testing.T) {
	catalog := os.Getenv(proxyHelperCatalogEnv)
	if catalog == "" {
		return
	}
	var filter []string
	if v := os.Getenv(proxyHelperServersEnv); v != "" {
		filter = strings.Split(v, ",")
	}
	opts := ProxyOptions{Only: os.Getenv("TETHER_PROXY_HELPER_ONLY") == "1", ServerFilter: filter, Confine: os.Getenv(proxyHelperConfineEnv) == "1", ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: os.Getenv("TETHER_PROXY_HELPER_MODE"), Source: "test"}}}}
	if raw := os.Getenv("TETHER_PROXY_HELPER_PROFILE_JSON"); raw != "" {
		var profile mcpgateway.Profile
		if err := json.Unmarshal([]byte(raw), &profile); err != nil {
			t.Fatal(err)
		}
		opts.Profile = mcpgateway.ProfileSelection{ID: "fixture", Source: "test", Profile: &profile}
	}
	if raw, set := os.LookupEnv(mcpgateway.ToolsEnv); set {
		profile, err := mcpgateway.ParseToolAllowlist(raw)
		if err != nil {
			t.Fatal(err)
		}
		opts.AuthorityProfiles = []mcpgateway.ProfileSelection{{Source: "boot tools", Profile: profile}}
	}
	adapter := newTestAdapter(t)
	adapter.svc.Catalog = &config.Catalog{}
	if os.Getenv("TETHER_PROXY_HELPER_PROTECTED") == "1" {
		adapter.protected = []string{catalog}
	}
	if err := adapter.RunWithProxyOpts(context.Background(), catalog, opts); err != nil {
		fmt.Fprintln(os.Stderr, "proxy helper:", err)
		os.Exit(3)
	}
}

// proxyCatalog writes one fixture upstream per name into a temporary catalog and
// returns its directory and the directory the fixtures record their starts in.
func proxyCatalog(t *testing.T, names ...string) (catalog, fixtures string) {
	t.Helper()
	catalog, fixtures = t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(catalog, "mcp-servers"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		b, err := yaml.Marshal(fixtureEntry(t, fixtures, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(catalog, "mcp-servers", name+".yaml"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return catalog, fixtures
}

// connectProxy runs the proxy helper with the given allow-list and returns a
// client connected to it.
func connectProxy(t *testing.T, catalog string, confine bool, servers ...string) *mcpsdk.ClientSession {
	return connectProxyMode(t, catalog, "flat", confine, servers...)
}
func connectProxyMode(t *testing.T, catalog, mode string, confine bool, servers ...string) *mcpsdk.ClientSession {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestRunWithProxyOptsHelper$")
	flag := "0"
	if confine {
		flag = "1"
	}
	cmd.Env = append(os.Environ(),
		"TETHER_PROXY_HELPER_MODE="+mode,
		proxyHelperCatalogEnv+"="+catalog,
		proxyHelperServersEnv+"="+strings.Join(servers, ","),
		proxyHelperConfineEnv+"="+flag,
		"GORACE=atexit_sleep_ms=0",
	)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "confine-test", Version: "test"}, nil).
		Connect(ctx, &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to the proxy: %v", err)
	}
	// Registered after the fixtures' own cleanup, so it runs first: the proxy
	// exits and takes its upstreams with it before they are waited for.
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func listToolNames(t *testing.T, cs *mcpsdk.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// tetherCall calls an upstream tool through tether_tool_call and returns its text and whether
// it was reported as an error.
func tetherCall(t *testing.T, cs *mcpsdk.ClientSession, tool string) (text string, isError bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "tether_tool_call",
		Arguments: map[string]any{"name": tool, "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatalf("tether_tool_call(%s): %v", tool, err)
	}
	return textOf(res), res.IsError
}

func started(fixtures, name string) bool {
	b, err := os.ReadFile(filepath.Join(fixtures, name+".events"))
	return err == nil && strings.Contains(string(b), "start")
}

// A server restriction applies to loading and every semantic call path,
// regardless of the former confine switch; there is no hybrid safety hatch.
func TestRunWithProxyOpts_RestrictionNeverStartsOrCallsUnlistedUpstreams(t *testing.T) {
	for _, confine := range []bool{false, true} {
		t.Run(fmt.Sprintf("confine=%v", confine), func(t *testing.T) {
			catalog, fixtures := proxyCatalog(t, "alpha", "beta", "gamma")
			cs := connectProxyMode(t, catalog, "search", confine, "alpha")
			if !started(fixtures, "alpha") {
				t.Fatal("granted upstream never started")
			}
			for _, name := range []string{"beta", "gamma"} {
				if started(fixtures, name) {
					t.Fatalf("excluded %s started", name)
				}
			}
			if text, isErr := tetherCall(t, cs, "beta_probe"); !isErr || !strings.Contains(text, "excluded") {
				t.Fatalf("excluded tool call: %v %s", isErr, text)
			}
			if text, isErr := tetherCall(t, cs, "alpha_probe"); isErr {
				t.Fatalf("granted call: %s", text)
			}
		})
	}
	t.Run("confined empty grants", func(t *testing.T) {
		catalog, fixtures := proxyCatalog(t, "alpha", "beta")
		cs := connectProxyMode(t, catalog, "search", true)
		for _, name := range []string{"alpha", "beta"} {
			if started(fixtures, name) {
				t.Fatalf("%s started with empty grants", name)
			}
		}
		if _, isErr := tetherCall(t, cs, "alpha_probe"); !isErr {
			t.Fatal("empty grants reachable")
		}
	})
}
