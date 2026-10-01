//go:build unix

package mcpadapter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	opts := ProxyOptions{ServerFilter: filter, Confine: os.Getenv(proxyHelperConfineEnv) == "1"}
	if err := newTestAdapter(t).RunWithProxyOpts(context.Background(), catalog, opts); err != nil {
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

// muxCall calls an upstream tool through mux_call and returns its text and whether
// it was reported as an error.
func muxCall(t *testing.T, cs *mcpsdk.ClientSession, tool string) (text string, isError bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "mux_call",
		Arguments: map[string]any{"tool_name": tool, "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatalf("mux_call(%s): %v", tool, err)
	}
	return textOf(res), res.IsError
}

func started(fixtures, name string) bool {
	b, err := os.ReadFile(filepath.Join(fixtures, name+".events"))
	return err == nil && strings.Contains(string(b), "start")
}

// With Confine, an upstream outside the allow-list is never started and cannot be
// reached through mux_call; without it the same allow-list only chooses which
// tools are native, and every upstream still starts. The second case is what
// makes the first one mean something.
func TestRunWithProxyOpts_ConfineKeepsUnlistedUpstreamsOut(t *testing.T) {
	t.Run("confined: only the listed upstream starts", func(t *testing.T) {
		catalog, fixtures := proxyCatalog(t, "alpha", "beta", "gamma")
		cs := connectProxy(t, catalog, true, "alpha")

		// The pool has finished starting by the time the proxy answers, so a
		// loaded upstream has already recorded its start.
		if !started(fixtures, "alpha") {
			t.Fatal("the granted upstream did not start")
		}
		for _, name := range []string{"beta", "gamma"} {
			if started(fixtures, name) {
				t.Fatalf("%s is not on the allow-list but was started: RunWithProxyOpts is not confining", name)
			}
		}
		tools := listToolNames(t, cs)
		if !slices.Contains(tools, "alpha_probe") {
			t.Fatalf("the granted upstream's tools are missing: %v", tools)
		}
		for _, tool := range tools {
			if strings.HasPrefix(tool, "beta_") || strings.HasPrefix(tool, "gamma_") {
				t.Fatalf("tool %q of an unlisted upstream is exposed", tool)
			}
		}
		if text, isErr := muxCall(t, cs, "beta_probe"); !isErr || !strings.Contains(text, "not found") {
			t.Fatalf("mux_call(beta_probe) should be not-found for an unlisted upstream, got isError=%v %q", isErr, text)
		}
		if text, isErr := muxCall(t, cs, "alpha_probe"); isErr {
			t.Fatalf("mux_call(alpha_probe) on the granted upstream failed: %q", text)
		}
	})

	t.Run("not confined: the list only chooses native tools, every upstream starts", func(t *testing.T) {
		catalog, fixtures := proxyCatalog(t, "alpha", "beta", "gamma")
		cs := connectProxy(t, catalog, false, "alpha")

		for _, name := range []string{"alpha", "beta", "gamma"} {
			if !started(fixtures, name) {
				t.Fatalf("%s did not start in an unconfined proxy", name)
			}
		}
		if text, isErr := muxCall(t, cs, "beta_probe"); isErr {
			t.Fatalf("an unconfined proxy reaches beta through mux_call, got error %q", text)
		}
	})

	t.Run("confined with an empty list: nothing starts", func(t *testing.T) {
		catalog, fixtures := proxyCatalog(t, "alpha", "beta")
		cs := connectProxy(t, catalog, true)

		for _, name := range []string{"alpha", "beta"} {
			if started(fixtures, name) {
				t.Fatalf("%s started under an empty allow-list", name)
			}
		}
		if _, isErr := muxCall(t, cs, "alpha_probe"); !isErr {
			t.Fatal("an empty allow-list must reach nothing")
		}
	})
}
