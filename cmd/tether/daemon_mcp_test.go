package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// This subprocess executes the production command tree/composition/run path,
// not a test-specific HTTP router. All authority-bearing paths are disposable.
func TestDaemonMCPCommandProcess(t *testing.T) {
	catalog := os.Getenv("TETHER_TEST_MCP_DAEMON_CATALOG")
	if catalog == "" {
		return
	}
	rootCmd.SetArgs([]string{"daemon", "run", "--catalog", catalog})
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestDaemonMCPUpstreamProcess(t *testing.T) {
	marker := os.Getenv("TETHER_TEST_MCP_CHILD_MARKER")
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
		os.Exit(2)
	}
	s := gomcp.NewServer("daemon-command-fixture", "1")
	s.RegisterTool(gomcp.Tool{Name: "daemon_fixture_echo", Description: "Test echo", InputSchema: gomcp.InputSchema(), ReadOnlyHint: true, Handler: func(context.Context, map[string]any) (any, error) { return "daemon child alive", nil }})
	if s.Run(context.Background()) != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestDaemonMCPRealCommandMountAndShutdown(t *testing.T) {
	if output, err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-user", "--unshare-pid", "--proc", "/proc", "--dev", "/dev", "--", "true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap unavailable: %v %s", err, output)
	}
	root, err := os.MkdirTemp(os.TempDir(), "dmcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home, catalog, run, state := filepath.Join(root, "home"), filepath.Join(root, "catalog"), filepath.Join(root, "run"), filepath.Join(root, "state")
	for _, path := range []string{home, catalog, run, state, filepath.Join(catalog, "mcp-servers")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("TETHER_TOKEN", "")
	addr := "unix:" + filepath.Join(run, "t.sock")
	dbPath := filepath.Join(state, "main.db")
	global := fmt.Sprintf("catalog:\n  defaults:\n    state_db: %q\ndaemon:\n  mcp_endpoint:\n    enabled: true\n  listen_addr: %q\n  pid_file: %q\n  shutdown_timeout: 3s\nidentity:\n  mode: observe\n  mcp_grants:\n    command-client:\n      servers: [app]\n", dbPath, addr, filepath.Join(run, "t.pid"))
	if err := os.WriteFile(filepath.Join(catalog, "global.yaml"), []byte(global), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "upstream.pid")
	entry := config.MCPServerEntry{ID: "app", Transport: "stdio", Command: executable, Args: []string{"-test.run=^TestDaemonMCPUpstreamProcess$"}, Env: map[string]string{"TETHER_TEST_MCP_CHILD_MARKER": marker}}
	raw, err := yaml.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalog, "mcp-servers", "app.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	token, err := identity.NewStore(db.DB()).Mint(context.Background(), identity.Principal{ID: "command-client", Kind: "service"})
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "daemon.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	child := exec.Command(executable, "-test.run=^TestDaemonMCPCommandProcess$")
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"} {
		if value, present := os.LookupEnv(key); present {
			child.Env = append(child.Env, key+"="+value)
		}
	}
	child.Env = append(child.Env, "TETHER_TEST_MCP_DAEMON_CATALOG="+catalog)
	child.Stdout, child.Stderr = log, log
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = child.Process.Kill()
			<-done
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dc := client.New(addr, client.WithToken(token))
	for dc.Ping(ctx) != nil {
		select {
		case err := <-done:
			stopped = true
			output, _ := os.ReadFile(logPath)
			t.Fatalf("daemon exited: %v\n%s", err, output)
		case <-ctx.Done():
			output, _ := os.ReadFile(logPath)
			t.Fatalf("daemon not ready: %s", output)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("production daemon eagerly started upstream")
	}
	session, err := dc.ConnectMCP(ctx, client.MCPOptions{})
	if err != nil {
		output, _ := os.ReadFile(logPath)
		t.Fatalf("MCP initialize: %v\n%s", err, output)
	}
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "daemon_fixture_echo"})
	if err != nil || result.IsError {
		t.Fatalf("daemon upstream: %+v %v", result, err)
	}
	result, err = session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_health"})
	if err != nil || result.IsError {
		t.Fatalf("daemon native gateway: %+v %v", result, err)
	}
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			output, _ := os.ReadFile(logPath)
			t.Fatalf("daemon shutdown: %v\n%s", err, output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MCP stream prevented daemon shutdown")
	}
}

func TestDaemonMCPEndpointOptInGuardsAuthority(t *testing.T) {
	for _, addr := range []string{"tcp::17881", "tcp:127.0.0.1:0"} {
		cat := &config.Catalog{}
		svc := &app.Service{Catalog: cat}
		cfg := daemon.Config{ListenAddr: addr, IdentityMode: identity.Enforce}
		h, err := buildDaemonMCP(context.Background(), svc, cfg, nil, nil)
		if err != nil || h != nil {
			t.Fatalf("disabled endpoint changes startup: %v %v", h, err)
		}
		cat.Global.Daemon.ListenAddr = addr
		if got := checkMCPEndpoint(cat); got.Status != statusOK {
			t.Fatal(got)
		}
		cat.Global.Daemon.MCPEndpoint.Enabled = true
		if _, err := buildDaemonMCP(context.Background(), svc, cfg, nil, nil); err == nil {
			t.Fatal("enabled bad authority accepted")
		}
		if got := checkMCPEndpoint(cat); got.Status != statusFail {
			t.Fatal(got)
		}
	}
}
