package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/spf13/cobra"
)

// stubDaemon answers GET /health on a unix socket, which is all a daemon-only
// `tether mcp` needs before it starts serving. It returns the catalog directory
// whose global.yaml points at that socket.
func stubDaemon(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	sock := filepath.Join(state, "tetherd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	router := http.NewServeMux()
	router.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{Handler: router}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	catalog := t.TempDir()
	global := "version: 1.0.0\ncatalog:\n  defaults:\n    state_db: " + filepath.Join(state, "tether.db") + "\n" +
		"daemon:\n  listen_addr: unix:" + sock + "\n  shutdown_timeout: 1s\n"
	if err := os.WriteFile(filepath.Join(catalog, "global.yaml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	return catalog
}

// setMCPFlags sets the package flag variables `tether mcp` reads, as the planted
// argv does, and restores them when the test ends.
func setMCPFlags(t *testing.T, proxy, daemonOnly, confine bool, servers string) {
	t.Helper()
	oldProxy, oldDaemonOnly, oldConfine, oldServers := mcpProxy, mcpDaemonOnly, mcpConfine, mcpServers
	oldBroker, oldOnly, oldToken, oldScopes := mcpBroker, mcpOnly, mcpToken, mcpScopes
	t.Cleanup(func() {
		mcpProxy, mcpDaemonOnly, mcpConfine, mcpServers = oldProxy, oldDaemonOnly, oldConfine, oldServers
		mcpBroker, mcpOnly, mcpToken, mcpScopes = oldBroker, oldOnly, oldToken, oldScopes
	})
	mcpProxy, mcpDaemonOnly, mcpConfine, mcpServers = proxy, daemonOnly, confine, servers
	mcpBroker, mcpOnly, mcpToken, mcpScopes = false, "", "tok", ""
}

// captureProxy replaces runProxy for the test and returns the options the
// command handed to it.
func captureProxy(t *testing.T) *mcpadapter.ProxyOptions {
	t.Helper()
	got := new(mcpadapter.ProxyOptions)
	old := runProxy
	t.Cleanup(func() { runProxy = old })
	runProxy = func(_ context.Context, _ *mcpadapter.Adapter, _ string, opts mcpadapter.ProxyOptions) error {
		*got = opts
		return nil
	}
	return got
}

// The regression test for the CW-20261001-0227 / CW-20261001-0173 merge: a
// launched agent's proxy is planted with `--confine --daemon-only`, and the
// daemon-only path built its own ProxyOptions without Confine. The flag was then
// accepted and ignored: every upstream started and tether_tool_call reached cerberus.
// This drives the real command entry with the planted flags and checks the
// options the proxy is started with, so omitting Confine at that call site fails.
func TestRunMCP_DaemonOnlyProxyIsConfined(t *testing.T) {
	catalog := stubDaemon(t)
	old := catalogPath
	catalogPath = catalog
	t.Cleanup(func() { catalogPath = old })

	for _, tc := range []struct {
		name    string
		confine bool
	}{
		{"planted flags: --confine --daemon-only", true},
		{"an unconfined daemon-only proxy stays unconfined", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setMCPFlags(t, true, true, tc.confine, "torque,tesseract")
			got := captureProxy(t)

			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			if err := runMCP(cmd, nil); err != nil {
				t.Fatalf("runMCP: %v", err)
			}
			if got.Confine != tc.confine {
				t.Fatalf("ProxyOptions.Confine = %v, want %v: the daemon-only path ignores --confine", got.Confine, tc.confine)
			}
			if want := []string{"torque", "tesseract"}; !reflect.DeepEqual(got.ServerFilter, want) {
				t.Fatalf("ServerFilter = %v, want %v", got.ServerFilter, want)
			}
			if _, ok := got.ProxyStore.(mcpadapter.DaemonProxyEvents); !ok {
				t.Fatalf("daemon-only path must record through the daemon, got %T", got.ProxyStore)
			}
		})
	}
}

// The operator's own in-process proxy is confined by the same flag.
func TestRunMCP_InProcessProxyIsConfined(t *testing.T) {
	catalog := stubDaemon(t)
	old := catalogPath
	catalogPath = catalog
	t.Cleanup(func() { catalogPath = old })
	setMCPFlags(t, true, false, true, "torque")
	got := captureProxy(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // stops the daemon event forwarder the command starts
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	if err := runMCP(cmd, nil); err != nil {
		t.Fatalf("runMCP: %v", err)
	}
	if !got.Confine || !reflect.DeepEqual(got.ServerFilter, []string{"torque"}) {
		t.Fatalf("in-process options = Confine %v, filter %v", got.Confine, got.ServerFilter)
	}
	if _, ok := got.ProxyStore.(mcpadapter.DaemonProxyEvents); ok {
		t.Fatal("the in-process path must write its own store, not the daemon's")
	}
}

func TestProxyOptionsFor_CarriesEverySwitchThatDecidesReach(t *testing.T) {
	got := proxyOptionsFor(true, []string{"a", "b"}, true, true)
	if !got.BrokerMode || !got.Only || !got.Confine || !reflect.DeepEqual(got.ServerFilter, []string{"a", "b"}) { //nolint:staticcheck // SA1019: asserting the deprecated field the flag still maps to
		t.Fatalf("proxyOptionsFor dropped a switch: %+v", got)
	}
}

// The planted server is told what to protect with --protect-path, in the mode
// every launched agent takes (--daemon-only) and in the operator's in-process
// one. A switch wired into one path only is a control that quietly does
// nothing on the other (CW-20261001-0227 was exactly that), so this drives the
// real command entry in both modes and reads the adapter it serves
// (CW-20261001-0142).
func TestRunMCP_ProtectPathReachesTheAdapterInBothModes(t *testing.T) {
	catalog := stubDaemon(t)
	old := catalogPath
	catalogPath = catalog
	t.Cleanup(func() { catalogPath = old })
	protected := t.TempDir()

	for _, tc := range []struct {
		name       string
		daemonOnly bool
	}{
		{"daemon-only, as a launched agent's", true},
		{"in-process, as the operator's", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setMCPFlags(t, true, tc.daemonOnly, true, "torque")
			oldProtect := mcpProtect
			t.Cleanup(func() { mcpProtect = oldProtect })
			mcpProtect = []string{protected}

			var served *mcpadapter.Adapter
			oldRun := runProxy
			t.Cleanup(func() { runProxy = oldRun })
			runProxy = func(_ context.Context, a *mcpadapter.Adapter, _ string, _ mcpadapter.ProxyOptions) error {
				served = a
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			if err := runMCP(cmd, nil); err != nil {
				t.Fatalf("runMCP: %v", err)
			}
			want, _ := filepath.EvalSymlinks(protected)
			if got := served.ProtectedPaths(); !reflect.DeepEqual(got, []string{want}) {
				t.Fatalf("the served adapter protects %q, want [%q]: --protect-path is ignored on this path", got, want)
			}
		})
	}
}
