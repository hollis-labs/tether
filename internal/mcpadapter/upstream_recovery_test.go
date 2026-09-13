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

	"github.com/hollis-labs/tether/internal/config"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// A real, disposable MCP process. Exit markers are consumed by the child;
// tests never signal another process or touch a user's catalog/state.
func TestUpstreamFixture(t *testing.T) {
	dir := os.Getenv("TETHER_UPSTREAM_FIXTURE")
	if dir == "" {
		return
	}
	name := os.Getenv("TETHER_UPSTREAM_NAME")
	appendEvent := func(event string) {
		f, err := os.OpenFile(filepath.Join(dir, name+".events"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(90)
		}
		_, _ = fmt.Fprintf(f, "%s %d\n", event, os.Getpid())
		_ = f.Close()
	}
	appendEvent("start")
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
					_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				}
				if string(b) == "stdout" {
					stdoutClosed.Store(true)
					_ = os.Stdout.Close()
					time.Sleep(400 * time.Millisecond)
					os.Exit(0)
				}
				code, _ := strconv.Atoi(string(b))
				os.Exit(code)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
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
			result = map[string]any{"protocolVersion": mcp.LATEST_PROTOCOL_VERSION, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": name, "version": "fixture"}}
		case "tools/list":
			defs := []mcp.Tool{mcp.NewTool(name+"_probe", mcp.WithDescription(name+" fixture probe"))}
			if _, err := os.Stat(filepath.Join(dir, name+".extra")); err == nil {
				defs = append(defs, mcp.NewTool(name+"_extra"))
			}
			result = map[string]any{"tools": defs}
		case "tools/call":
			appendEvent("call")
			if req.Params.Arguments["hold"] == true {
				appendEvent("side_effect")
				select {}
			}
			result = mcp.NewToolResultText(strconv.Itoa(os.Getpid()))
		default:
			result = map[string]any{}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	appendEvent("eof")
	if stdoutClosed.Load() {
		time.Sleep(500 * time.Millisecond)
	}
	os.Exit(0)
}

func fixtureEntry(t *testing.T, dir, name string) config.MCPServerEntry {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return config.MCPServerEntry{ID: name, Transport: "stdio", Command: exe, Args: []string{"-test.run=^TestUpstreamFixture$"}, Tags: []string{name}, Env: map[string]string{"TETHER_UPSTREAM_FIXTURE": dir, "TETHER_UPSTREAM_NAME": name, "FIXTURE_TOKEN": "secret-fixture-token", "GORACE": "atexit_sleep_ms=0"}}
}

func awaitStatus(t *testing.T, p *ClientPool, id string, predicate func(ServerStatus) bool) ServerStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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
	res, err := r.Handle(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name + "_probe"}})
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
			pool.policy.delays = []time.Duration{150 * time.Millisecond, 300 * time.Millisecond}
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
			local := server.NewMCPServer("proxy", "test", server.WithToolCapabilities(true))
			adapter.registerTools(local)
			idx := NewDiscoveryIndex()
			tags := map[string][]string{"alpha": {"alpha"}, "beta": {"beta"}}
			idx.Build(registry, tags)
			live := &liveProxyCatalog{adapter: adapter, server: local, registry: registry, router: router, index: idx, serverTags: tags, firehose: true}
			live.addProxyTools(registry.AllDefinitions()...)
			pool.SetToolRefreshHandler(live.applyRefresh)
			adapter.registerDiscoverTool(local, idx, nil, true)
			adapter.registerSemanticDiscoverTool(local, idx, nil, true)
			adapter.registerMCPServersTool(local, pool, pool.entries, nil, true)
			downstream, err := mcpclient.NewInProcessClient(local)
			if err != nil {
				t.Fatal(err)
			}
			defer downstream.Close()
			if err := downstream.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := downstream.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
				t.Fatal(err)
			}
			callBody := func(name string, args map[string]any) map[string]any {
				res, err := downstream.CallTool(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
				if err != nil {
					t.Fatal(err)
				}
				return parseToolJSON(t, res)
			}
			pending := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, err := router.Handle(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "alpha_probe", Arguments: map[string]any{"hold": true}}})
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
			case <-time.After(time.Second):
				t.Fatal("in-flight call hung")
			}
			s := awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "reconnecting" })
			if s.LastExit == nil {
				t.Fatal("missing exit details")
			}
			want := map[string]string{"0": "clean", "23": "error", "signal": "signal"}[kind]
			if s.LastExit.Kind != want {
				t.Fatalf("exit: %+v", s.LastExit)
			}
			if kind == "23" && s.LastExit.Code != 23 {
				t.Fatalf("exit code: %+v", s.LastExit)
			}
			if !strings.Contains(s.StderrTail, "fixture diagnostic") || strings.Contains(s.StderrTail, "secret-fixture-token") {
				t.Fatalf("stderr: %q", s.StderrTail)
			}
			if health := callBody("mux_health", nil); health["ok"] != false {
				t.Fatalf("health hid failure: %+v", health)
			}
			for _, tool := range []string{"mux_discover", "mux_discover_tools"} {
				body := callBody(tool, map[string]any{"intent": "alpha"})
				if body["complete"] != false || body["count"] != float64(0) || len(body["unavailable_servers"].([]any)) != 1 {
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
			assertToolPresent(context.Background(), t, downstream, "alpha_extra")
			if health := callBody("mux_health", nil); health["ok"] != true {
				t.Fatalf("health did not recover: %+v", health)
			}
			if body := callBody("mux_discover", map[string]any{"intent": "no-such-tool-zxy"}); body["complete"] != true || body["count"] != float64(0) {
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
	pool.policy.delays = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}
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
			case <-time.After(time.Second):
				t.Fatal("fixture not reaped on EOF")
			}
		}
	}
}

func TestUpstreamRecovery_StdoutCloseDoesNotSpawnOverLiveProcess(t *testing.T) {
	dir := t.TempDir()
	pool := NewClientPool([]config.MCPServerEntry{fixtureEntry(t, dir, "alpha")}, NewToolRegistry())
	pool.policy.delays = []time.Duration{20 * time.Millisecond}
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	fixtureMarker(t, dir, "alpha", "stdout")
	awaitStatus(t, pool, "alpha", func(s ServerStatus) bool {
		return s.Status == "failed" && strings.Contains(s.Error, "waiting for upstream process exit")
	})
	time.Sleep(100 * time.Millisecond)
	b, _ := os.ReadFile(filepath.Join(dir, "alpha.events"))
	if strings.Count(string(b), "start") != 1 {
		t.Fatalf("duplicate live process: %s", b)
	}
	awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "connected" && s.RestartAttempts == 1 })
}

func TestUpstreamRecovery_StableBudgetResetAndCancelBackoff(t *testing.T) {
	dir := t.TempDir()
	pool := NewClientPool([]config.MCPServerEntry{fixtureEntry(t, dir, "alpha")}, NewToolRegistry())
	pool.policy.delays = []time.Duration{150 * time.Millisecond}
	pool.policy.stableFor = 200 * time.Millisecond
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
	time.Sleep(200 * time.Millisecond)
	b, _ := os.ReadFile(filepath.Join(dir, "alpha.events"))
	if strings.Count(string(b), "start") != 2 {
		t.Fatalf("respawned during shutdown: %s", b)
	}
}

func TestUpstreamRecovery_OldRefreshCannotReplaceRecoveredClient(t *testing.T) {
	registry := NewToolRegistry()
	pool := NewClientPool(nil, registry)
	entered, release := make(chan struct{}), make(chan struct{})
	old := &mockClient{listToolsFunc: func(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
		close(entered)
		<-release
		return &mcp.ListToolsResult{Tools: []mcp.Tool{makeTool("obsolete")}}, nil
	}}
	current := &mockClient{}
	pool.statuses["alpha"] = &clientStatus{entry: config.MCPServerEntry{ID: "alpha"}, client: old, state: "connected"}
	finished := make(chan error, 1)
	go func() { _, err := pool.RefreshServer(context.Background(), "alpha"); finished <- err }()
	<-entered
	pool.mu.Lock()
	pool.statuses["alpha"].client = current
	pool.mu.Unlock()
	if _, err := pool.publish(context.Background(), "alpha", current, []mcp.Tool{makeTool("current")}, true); err != nil {
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
		tail := stderrTail{secrets: secrets}
		prefix := secret[:len(secret)-1]
		_, _ = tail.Write([]byte(strings.Repeat("x", stderrTailBytes) + "connector authorization error: " + prefix))

		betweenWrites := tail.String()
		if len(betweenWrites) > stderrTailBytes || strings.Contains(betweenWrites, prefix) || !strings.HasSuffix(betweenWrites, "[redacted]") {
			t.Fatalf("intermediate stderr snapshot exposed credential prefix for values %q: length=%d tail=%q", secrets, len(betweenWrites), betweenWrites[len(betweenWrites)-64:])
		}

		_, _ = tail.Write([]byte(secret[len(secret)-1:] + " diagnostic"))
		complete := tail.String()
		if len(complete) > stderrTailBytes || strings.Contains(complete, secret) || !strings.HasSuffix(complete, "[redacted] diagnostic") {
			t.Fatalf("completed stderr snapshot exposed credential for values %q: length=%d tail=%q", secrets, len(complete), complete[len(complete)-64:])
		}
	}

	// Also cover a retained window beginning partway through a credential.
	tail := stderrTail{secrets: []string{"1", secret}}
	_, _ = tail.Write([]byte("discarded-prefix" + secret + strings.Repeat("y", stderrTailBytes-len(secret)+1)))
	truncated := tail.String()
	if len(truncated) > stderrTailBytes || strings.HasPrefix(truncated, secret[1:]) {
		t.Fatalf("truncated stderr snapshot exposed credential suffix: length=%d head=%q", len(truncated), truncated[:64])
	}
}
