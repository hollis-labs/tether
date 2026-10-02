package mcpadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	"github.com/hollis-labs/go-mcp/supervise"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

// A real, disposable MCP process. Exit markers are consumed by the child;
// tests never signal another process or touch a user's catalog/state.
func TestUpstreamFixture(t *testing.T) {
	dir := os.Getenv("TETHER_UPSTREAM_FIXTURE")
	if dir == "" {
		return
	}
	name := os.Getenv("TETHER_UPSTREAM_NAME")
	if name == "" {
		for i, arg := range os.Args {
			if arg == "--" && i+1 < len(os.Args) {
				name = os.Args[i+1]
				break
			}
		}
	}
	appendEvent := func(event string) {
		f, err := os.OpenFile(filepath.Join(dir, name+".events"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(90)
		}
		_, _ = fmt.Fprintf(f, "%s %d\n", event, os.Getpid())
		_ = f.Close()
	}
	appendEvent("start")
	if os.Getenv("TETHER_UPSTREAM_RECORD_TOKEN") == "1" {
		if err := os.WriteFile(filepath.Join(dir, name+".token"), []byte(os.Getenv("TETHER_TOKEN")), 0600); err != nil {
			os.Exit(93)
		}
	}
	_, _ = fmt.Fprintln(os.Stderr, "fixture diagnostic secret-fixture-token")
	var stdoutClosed atomic.Bool
	if os.Getenv("TETHER_UPSTREAM_STARTUP_FAIL") == "1" {
		os.Exit(23)
	}
	go func() {
		for {
			b, err := os.ReadFile(filepath.Join(dir, name+".exit"))
			if err == nil {
				_ = os.Remove(filepath.Join(dir, name+".exit"))
				appendEvent("exit")
				if string(b) == "signal" {
					if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
						os.Exit(91)
					}
					select {}
				}
				if string(b) == "stdout" {
					stdoutClosed.Store(true)
					_ = os.Stdout.Close()
					awaitFixtureRelease(dir, name)
					os.Exit(0)
				}
				code, _ := strconv.Atoi(string(b))
				os.Exit(code)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if os.Getenv("TETHER_UPSTREAM_HANG") == "1" {
		select {}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			if os.Getenv("TETHER_UPSTREAM_RECORD_INITIALIZE") == "1" {
				if err := os.WriteFile(filepath.Join(dir, name+".initialize.json"), scanner.Bytes(), 0600); err != nil {
					os.Exit(92)
				}
			}
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": name, "version": "fixture"}}
		case "tools/list":
			toolName := name + "_probe"
			if v := os.Getenv("TETHER_UPSTREAM_TOOL_NAME"); v != "" {
				toolName = v
			}
			if raw, err := os.ReadFile(filepath.Join(dir, name+".tool")); err == nil {
				toolName = string(raw)
			}
			defs := []map[string]any{{
				"name":        toolName,
				"description": name + " fixture probe",
				"inputSchema": map[string]any{"type": "object"},
			}}
			if _, err := os.Stat(filepath.Join(dir, name+".extra")); err == nil {
				defs = append(defs, map[string]any{
					"name":        name + "_extra",
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			result = map[string]any{"tools": defs}
		case "tools/call":
			appendEvent("call")
			if expected := os.Getenv("TETHER_UPSTREAM_EXPECT_NAME"); expected != "" && req.Params.Name != expected {
				result = map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "wrong forwarded name"}}}
				break
			}
			if req.Params.Arguments["hold"] == true {
				appendEvent("side_effect")
				select {}
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": strconv.Itoa(os.Getpid())}}}
		default:
			result = map[string]any{}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	appendEvent("eof")
	if stdoutClosed.Load() {
		awaitFixtureRelease(dir, name)
	}
	os.Exit(0)
}

// awaitFixtureRelease holds a fixture that has closed its stdout alive until
// the test writes <name>.release (releaseFixture). A fixed sleep here let a
// loaded runner skip the whole "stdout closed, process still alive" window
// the test needs to observe. The cap only bounds an abandoned fixture.
func awaitFixtureRelease(dir, name string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, name+".release")); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func releaseFixture(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

// awaitFixturesExited waits until every fixture process recorded in
// <name>.events has exited, killing any still alive at the deadline.
// ClientPool.Shutdown closes an upstream's stdin without waiting for it to
// exit, so without this a fixture could still be writing its events file
// while the test's TempDir is removed ("directory not empty").
func awaitFixturesExited(t *testing.T, dir, name string) {
	t.Helper()
	pids := func() []int {
		b, _ := os.ReadFile(filepath.Join(dir, name+".events"))
		var out []int
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "start" {
				if pid, err := strconv.Atoi(f[1]); err == nil {
					out = append(out, pid)
				}
			}
		}
		return out
	}
	alive := func(pid int) bool { return syscall.Kill(pid, 0) == nil }
	deadline := time.Now().Add(10 * time.Second)
	for {
		var live []int
		for _, pid := range pids() {
			if alive(pid) {
				live = append(live, pid)
			}
		}
		if len(live) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, pid := range live {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			t.Logf("killed fixture processes still alive after the test: %v", live)
			time.Sleep(100 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fixtureEntry(t *testing.T, dir, name string) config.MCPServerEntry {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Registered after the caller's t.TempDir, so it runs before the
	// directory is removed.
	t.Cleanup(func() { awaitFixturesExited(t, dir, name) })
	return config.MCPServerEntry{ID: name, Transport: "stdio", Command: exe, Args: []string{"-test.run=^TestUpstreamFixture$", "--", name}, Tags: []string{name}, Env: map[string]string{"TETHER_UPSTREAM_FIXTURE": dir, "FIXTURE_TOKEN": "secret-fixture-token", "GORACE": "atexit_sleep_ms=0"}}
}

func awaitStatus(t *testing.T, p *ClientPool, id string, predicate func(ServerStatus) bool) ServerStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range p.StatusSummary() {
			if s.ID == id && predicate(s) {
				return s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status timeout: %+v", p.StatusSummary())
	return ServerStatus{}
}

func fixtureMarker(t *testing.T, dir, name, kind string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".exit"), []byte(kind), 0600); err != nil {
		t.Fatal(err)
	}
}

func probePID(t *testing.T, r *ProxyRouter, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := r.Handle(ctx, ToolCall{ToolName: name + "_probe"})
	if err != nil || res.IsError {
		t.Fatalf("probe %s: %+v %v", name, res, err)
	}
	return textOf(res)
}

func TestUpstreamRecovery_ExitKindsInflightSiblingAndVisibility(t *testing.T) {
	for _, kind := range []string{"0", "23", "signal"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			registry := NewToolRegistry()
			pool := NewClientPool([]config.MCPServerEntry{fixtureEntry(t, dir, "alpha"), fixtureEntry(t, dir, "beta")}, registry)
			// Long enough that a loaded runner still observes, and makes its
			// health and discovery calls inside, the reconnecting window.
			pool.policy.Delays = []time.Duration{time.Second, 2 * time.Second}
			if err := pool.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer pool.Shutdown()
			router := NewProxyRouter(registry)
			router.pool = pool
			alpha, beta := probePID(t, router, "alpha"), probePID(t, router, "beta")
			adapter := newTestAdapter(t)
			adapter.svc.Catalog = &config.Catalog{}
			adapter.upstreams = pool
			local := gomcp.NewServer("proxy", "test")
			adapter.registerTools(local)
			tags := map[string][]string{"alpha": {"alpha"}, "beta": {"beta"}}
			live := &liveProxyCatalog{adapter: adapter, server: local, registry: registry, router: router, firehose: true}
			live.addProxyTools(registry.AllDefinitions()...)
			pool.SetToolRefreshHandler(live.applyRefresh)
			gateway := adapter.gatewayService(registry, router, mcpgateway.Selection{Mode: mcpgateway.Search, Source: "test"}, tags)
			adapter.registerSearchTool(local, gateway)
			adapter.registerListTool(local, gateway)
			adapter.registerGatewayStatus(local, gateway)
			downstream := connectInMemory(t, local)
			callBody := func(name string, args map[string]any) map[string]any {
				res, err := downstream.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				return parseToolJSON(t, res)
			}
			pending := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, err := router.Handle(ctx, ToolCall{ToolName: "alpha_probe", Args: map[string]any{"hold": true}})
				pending <- err
			}()
			deadline := time.Now().Add(time.Second)
			for {
				b, _ := os.ReadFile(filepath.Join(dir, "alpha.events"))
				if strings.Contains(string(b), "side_effect") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("in-flight call not received")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := os.WriteFile(filepath.Join(dir, "alpha.extra"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			fixtureMarker(t, dir, "alpha", kind)
			select {
			case err := <-pending:
				if err == nil || !strings.Contains(err.Error(), "not replayed") {
					t.Fatalf("pending error: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("in-flight call hung")
			}
			s := awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "reconnecting" })
			if s.LastExit == nil {
				t.Fatal("missing exit details")
			}
			want := map[string]string{"0": "clean", "23": "error", "signal": "signal"}[kind]
			if string(s.LastExit.Kind) != want {
				t.Fatalf("exit: %+v", s.LastExit)
			}
			if kind == "23" && s.LastExit.Code != 23 {
				t.Fatalf("exit code: %+v", s.LastExit)
			}
			if !strings.Contains(s.StderrTail, "fixture diagnostic") || strings.Contains(s.StderrTail, "secret-fixture-token") {
				t.Fatalf("stderr: %q", s.StderrTail)
			}
			if health := callBody("tether_health", nil); health["ok"] != false {
				t.Fatalf("health hid failure: %+v", health)
			}
			for _, tool := range []string{"tether_tool_search", "tether_tool_list"} {
				args := map[string]any{"query": "alpha"}
				if tool == "tether_tool_list" {
					args = map[string]any{"servers": []string{"alpha"}}
				}
				body := callBody(tool, args)
				if body["complete"] != false || body["returned"] != float64(0) || len(body["unavailable_servers"].([]any)) != 1 {
					t.Fatalf("discovery hid failure: %+v", body)
				}
			}
			if got := probePID(t, router, "beta"); got != beta {
				t.Fatal("sibling replaced during recovery")
			}
			awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "connected" && s.RestartAttempts == 1 })
			if got := probePID(t, router, "alpha"); got == alpha {
				t.Fatal("dead client reused")
			}
			if got := probePID(t, router, "beta"); got != beta {
				t.Fatal("sibling replaced")
			}
			// Connected status precedes the downstream catalog refresh.
			waitForTool(context.Background(), t, downstream, "alpha_extra")
			if health := callBody("tether_health", nil); health["ok"] != true {
				t.Fatalf("health did not recover: %+v", health)
			}
			if body := callBody("tether_tool_search", map[string]any{"query": "no-such-tool-zxy"}); body["complete"] != true || body["returned"] != float64(0) {
				t.Fatalf("no-match: %+v", body)
			}
			b, _ := os.ReadFile(filepath.Join(dir, "alpha.events"))
			if strings.Count(string(b), "side_effect") != 1 {
				t.Fatalf("call replayed: %s", b)
			}
			t.Logf("%s: in-flight released and recovery completed in %s; sibling PID %s unchanged", kind, time.Since(started), beta)
		})
	}
}

func TestUpstreamRecovery_CrashLoopBoundAndShutdown(t *testing.T) {
	dir := t.TempDir()
	entry := fixtureEntry(t, dir, "alpha")
	entry.Env["TETHER_UPSTREAM_STARTUP_FAIL"] = "1"
	pool := NewClientPool([]config.MCPServerEntry{entry, fixtureEntry(t, dir, "beta")}, NewToolRegistry())
	pool.policy.Delays = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	awaitStatus(t, pool, "alpha", func(s ServerStatus) bool {
		return s.Status == "failed" && s.RestartAttempts == 3 && s.NextRetryAt == nil
	})
	time.Sleep(180 * time.Millisecond)
	b, _ := os.ReadFile(filepath.Join(dir, "alpha.events"))
	if strings.Count(string(b), "start") != 4 {
		t.Fatalf("unbounded/repeated starts: %s", b)
	}
	router := NewProxyRouter(pool.registry)
	router.pool = pool
	_ = probePID(t, router, "beta")
	pool.Shutdown()
	for _, s := range pool.statuses {
		if leaf, ok := s.client.(*stdioUpstream); ok {
			select {
			case <-leaf.done:
			case <-time.After(10 * time.Second):
				t.Fatal("fixture not reaped on EOF")
			}
		}
	}
}

func TestUpstreamRecovery_StdoutCloseDoesNotSpawnOverLiveProcess(t *testing.T) {
	dir := t.TempDir()
	pool := NewClientPool([]config.MCPServerEntry{fixtureEntry(t, dir, "alpha")}, NewToolRegistry())
	pool.policy.Delays = []time.Duration{20 * time.Millisecond}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	fixtureMarker(t, dir, "alpha", "stdout")
	// The fixture stays alive with its stdout closed until released, so this
	// state lasts as long as the test needs it to.
	awaitStatus(t, pool, "alpha", func(s ServerStatus) bool {
		return s.Status == "failed" && strings.Contains(s.Error, "waiting for upstream process exit")
	})
	time.Sleep(100 * time.Millisecond)
	b, _ := os.ReadFile(filepath.Join(dir, "alpha.events"))
	if strings.Count(string(b), "start") != 1 {
		t.Fatalf("duplicate live process: %s", b)
	}
	releaseFixture(t, dir, "alpha")
	awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "connected" && s.RestartAttempts == 1 })
}

func TestUpstreamRecovery_StableBudgetResetAndCancelBackoff(t *testing.T) {
	dir := t.TempDir()
	pool := NewClientPool([]config.MCPServerEntry{fixtureEntry(t, dir, "alpha")}, NewToolRegistry())
	// The backoff is the window the test must observe as "reconnecting" and
	// shut down inside; long enough that a loaded runner cannot miss it.
	pool.policy.Delays = []time.Duration{time.Second}
	pool.policy.StableFor = 200 * time.Millisecond
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	fixtureMarker(t, dir, "alpha", "0")
	awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "connected" && s.RestartAttempts == 1 })
	awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "connected" && s.RestartAttempts == 0 })
	fixtureMarker(t, dir, "alpha", "23")
	awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "reconnecting" })
	pool.Shutdown()
	// Outlast the backoff, so a restart Shutdown failed to cancel would
	// have happened by now.
	time.Sleep(1500 * time.Millisecond)
	b, _ := os.ReadFile(filepath.Join(dir, "alpha.events"))
	if strings.Count(string(b), "start") != 2 {
		t.Fatalf("respawned during shutdown: %s", b)
	}
}

func TestUpstreamRecovery_OldRefreshCannotReplaceRecoveredClient(t *testing.T) {
	registry := NewToolRegistry()
	pool := NewClientPool(nil, registry)
	entered, release := make(chan struct{}), make(chan struct{})
	old := &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
		close(entered)
		<-release
		return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool("obsolete")}}, nil
	}}
	current := &mockClient{}
	pool.statuses["alpha"] = &clientStatus{entry: config.MCPServerEntry{ID: "alpha"}, client: old, state: "connected"}
	finished := make(chan error, 1)
	go func() { _, err := pool.RefreshServer(context.Background(), "alpha"); finished <- err }()
	<-entered
	pool.mu.Lock()
	pool.statuses["alpha"].client = current
	pool.mu.Unlock()
	if _, err := pool.publish(context.Background(), "alpha", current, []*mcpsdk.Tool{makeTool("current")}, true); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-finished; err == nil {
		t.Fatal("stale refresh accepted")
	}
	if _, ok := registry.Lookup("obsolete"); ok {
		t.Fatal("old tools restored")
	}
	rt, ok := registry.Lookup("current")
	if !ok || rt.Client != current {
		t.Fatal("recovered client overwritten")
	}
}

func TestUpstreamStderr_BoundedAcrossWritesAndRedacted(t *testing.T) {
	const secret = "fake-review-token-0123456789"
	for _, secrets := range [][]string{{"1", secret}, {secret, "1"}} {
		tail := supervise.Tail{Secrets: secrets}
		prefix := secret[:len(secret)-1]
		_, _ = tail.Write([]byte(strings.Repeat("x", supervise.DefaultTailBytes) + "connector authorization error: " + prefix))

		betweenWrites := tail.String()
		if len(betweenWrites) > supervise.DefaultTailBytes || strings.Contains(betweenWrites, prefix) || !strings.HasSuffix(betweenWrites, "[redacted]") {
			t.Fatalf("intermediate stderr snapshot exposed credential prefix for values %q: length=%d tail=%q", secrets, len(betweenWrites), betweenWrites[len(betweenWrites)-64:])
		}

		_, _ = tail.Write([]byte(secret[len(secret)-1:] + " diagnostic"))
		complete := tail.String()
		if len(complete) > supervise.DefaultTailBytes || strings.Contains(complete, secret) || !strings.HasSuffix(complete, "[redacted] diagnostic") {
			t.Fatalf("completed stderr snapshot exposed credential for values %q: length=%d tail=%q", secrets, len(complete), complete[len(complete)-64:])
		}
	}

	// Also cover a retained window beginning partway through a credential.
	tail := supervise.Tail{Secrets: []string{"1", secret}}
	_, _ = tail.Write([]byte("discarded-prefix" + secret + strings.Repeat("y", supervise.DefaultTailBytes-len(secret)+1)))
	truncated := tail.String()
	if len(truncated) > supervise.DefaultTailBytes || strings.HasPrefix(truncated, secret[1:]) {
		t.Fatalf("truncated stderr snapshot exposed credential suffix: length=%d head=%q", len(truncated), truncated[:64])
	}
}

func TestUpstreamStderr_RedactsResolvedArgumentSecretsOnly(t *testing.T) {
	t.Setenv("TEST_MCP_ARG_SECRET", "resolved-argument-secret")
	dir := t.TempDir()
	serverDir := filepath.Join(dir, "mcp-servers")
	if err := os.MkdirAll(serverDir, 0o700); err != nil {
		t.Fatal(err)
	}
	catalog := []byte("id: test\ntransport: stdio\ncommand: /bin/test\nargs: [mcp, '${TEST_MCP_ARG_SECRET}']\n")
	if err := os.WriteFile(filepath.Join(serverDir, "test.yaml"), catalog, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := config.LoadMCPServers(dir)
	if err != nil {
		t.Fatal(err)
	}
	values := stderrRedactionValues(entries[0])
	if got := supervise.Redact("argument resolved-argument-secret; ordinary mcp", values); got != "argument [redacted]; ordinary mcp" {
		t.Fatalf("stderr redaction = %q", got)
	}
}
