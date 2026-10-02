package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func daemonTestRoots(t *testing.T) DaemonProtectedRoots {
	t.Helper()
	root := t.TempDir()
	r := DaemonProtectedRoots{Catalog: filepath.Join(root, "catalog"), Run: filepath.Join(root, "run"), State: filepath.Join(root, "state")}
	for _, path := range []string{r.Catalog, r.Run, r.State} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestDaemonUpstreamFixture(t *testing.T) {
	outside := os.Getenv("TETHER_DAEMON_FIXTURE")
	if outside == "" {
		return
	}
	if os.Getenv("DAEMON_ONLY_SECRET") != "" || os.Getenv("TETHER_TOKEN") != "" || os.Getenv("CATALOG_FIXTURE_SECRET") != "explicit-fixture-value" {
		os.Exit(93)
	}
	var roots []string
	if err := json.Unmarshal([]byte(os.Getenv("TETHER_DAEMON_PROTECTED")), &roots); err != nil {
		os.Exit(90)
	}
	f, err := os.OpenFile(filepath.Join(outside, "starts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(91)
	}
	_, _ = fmt.Fprintln(f, os.Getpid())
	_ = f.Close()
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fixture", Version: "test"}, nil)
	s.AddTool(&mcpsdk.Tool{Name: "fixture_write", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		blocked := true
		for _, root := range roots {
			if err := os.WriteFile(filepath.Join(root, "sentinel"), []byte("corrupted"), 0600); err == nil {
				blocked = false
			}
		}
		err := os.WriteFile(filepath.Join(outside, "allowed"), []byte("ok"), 0600)
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf("blocked=%t outside=%t", blocked, err == nil)}}}, nil
	})
	if err := s.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		os.Exit(92)
	}
	os.Exit(0)
}

func TestSharedUpstreams_ConfinedOneProcessForConcurrentViews(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DAEMON_ONLY_SECRET", "must-not-be-delegated")
	t.Setenv("TETHER_TOKEN", "must-not-be-delegated")
	if runtime.GOOS != "linux" {
		t.Skip("real protect-only upstream smoke requires Linux bubblewrap")
	}
	// Probe the host, not our confinement implementation. Ubuntu CI may install
	// bwrap while AppArmor still denies namespaces. The refusal/no-fallback test
	// remains mandatory; this real namespace smoke runs on capable hosts.
	probe := exec.Command("bwrap", "--bind", "/", "/", "--unshare-user", "--unshare-pid", "--proc", "/proc", "/bin/true")
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("host cannot create bubblewrap namespace: %v (%s)", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	roots := daemonTestRoots(t)
	paths, err := roots.paths()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if err := os.WriteFile(filepath.Join(path, "sentinel"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	raw, _ := json.Marshal(paths)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewSharedUpstreams([]config.MCPServerEntry{{ID: "fixture", Transport: "stdio", Command: executable, Args: []string{"-test.run=^TestDaemonUpstreamFixture$"}, Env: map[string]string{"TETHER_DAEMON_FIXTURE": outside, "TETHER_DAEMON_PROTECTED": string(raw), "CATALOG_FIXTURE_SECRET": "explicit-fixture-value"}}}, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Start(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	status := r.pool.StatusSummary()
	if len(status) != 1 || status[0].Status != "connected" {
		t.Fatalf("sandboxed upstream failed: %+v", status)
	}
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := r.OpenView("verified session", []string{"fixture"}, mcpgateway.Selection{Mode: mcpgateway.Flat})
			if err != nil {
				t.Error(err)
				return
			}
			res, err := v.Service.Call(ctx, "fixture_write", map[string]any{}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if res.IsError || len(res.Content) != 1 || res.Content[0].(*mcpsdk.TextContent).Text != "blocked=true outside=true" {
				t.Errorf("write confinement result: %+v", res)
			}
		}()
	}
	wg.Wait()
	starts, err := os.ReadFile(filepath.Join(outside, "starts"))
	if err != nil || len(strings.Fields(string(starts))) != 1 {
		t.Fatalf("expected one upstream process, starts=%q err=%v", starts, err)
	}
	for _, path := range paths {
		b, err := os.ReadFile(filepath.Join(path, "sentinel"))
		if err != nil || string(b) != "original" {
			t.Fatalf("protected file changed: %s %q %v", path, b, err)
		}
	}
	empty, err := r.OpenView("empty", nil, mcpgateway.Selection{Mode: mcpgateway.Search})
	if err != nil {
		t.Fatal(err)
	}
	list, err := empty.Service.List(mcpgateway.Request{})
	if err != nil || len(list.Items) != 0 || len(list.UnavailableServers) != 0 {
		t.Fatalf("empty view=%+v err=%v", list, err)
	}
	if _, err := empty.Service.Call(ctx, "fixture_write", nil, nil); err == nil {
		t.Fatal("empty grants dispatched")
	}
	if _, err := empty.Refresh(ctx, "fixture"); err == nil {
		t.Fatal("empty grants refreshed hidden upstream")
	}
}

func TestDaemonProtectedRoots_RequireAllRoots(t *testing.T) {
	roots := daemonTestRoots(t)
	for _, bad := range []DaemonProtectedRoots{{}, {Catalog: roots.Catalog, Run: roots.Run}, {Catalog: roots.Catalog, Run: roots.Run, State: "relative"}, {Catalog: roots.Catalog, Run: roots.Run, State: filepath.Join(roots.State, "missing")}} {
		if _, err := NewSharedUpstreams(nil, bad); err == nil {
			t.Fatal("unprotected daemon pool accepted", bad)
		}
	}
}

func TestSharedUpstreams_SubscriberFanout(t *testing.T) {
	r, err := NewSharedUpstreams(nil, daemonTestRoots(t))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var calls [2]int
	unsubscribe := r.Subscribe(func(ToolRefreshResult) { calls[0]++ })
	r.Subscribe(func(ToolRefreshResult) { calls[1]++ })
	r.publish(ToolRefreshResult{ServerID: "one"})
	unsubscribe()
	r.publish(ToolRefreshResult{ServerID: "two"})
	if calls != [2]int{1, 2} {
		t.Fatal(calls)
	}
}

func TestDaemonUpstreamConfinement_NoUnconfinedFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap failure path")
	}
	t.Setenv("HOME", t.TempDir())
	paths, err := daemonTestRoots(t).paths()
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "") // mandatory sandbox executable unavailable
	entry := config.MCPServerEntry{ID: "fixture", Command: executable, Args: []string{"-test.run=^TestDaemonUpstreamFixture$"}, Env: map[string]string{"TETHER_DAEMON_FIXTURE": outside, "TETHER_DAEMON_PROTECTED": "[]"}}
	child, transport, err := spawnStdioUpstreamConfined(entry, paths)
	if err == nil || child != nil || transport != nil {
		t.Fatalf("sandbox failure fell back: %v %v %v", child, transport, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "starts")); !os.IsNotExist(err) {
		t.Fatalf("unconfined fixture started: %v", err)
	}
}

func TestSharedUpstreams_RemoteExclusionVisible(t *testing.T) {
	r, err := NewSharedUpstreams([]config.MCPServerEntry{{ID: "remote", Transport: "http", URL: "http://127.0.0.1:1/private-secret"}}, daemonTestRoots(t))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, err := r.OpenView("verified principal", []string{"remote"}, mcpgateway.Selection{Mode: mcpgateway.Flat})
	if err != nil {
		t.Fatal(err)
	}
	s := v.Service.Snapshot()
	if len(s.Origins) != 1 || s.Origins[0].Status != "excluded" || !strings.Contains(s.Origins[0].Error, "cannot be confined locally") || strings.Contains(s.Origins[0].Error, "private-secret") {
		t.Fatalf("remote status: %+v", s)
	}
}
