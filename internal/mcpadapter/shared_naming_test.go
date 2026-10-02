package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSharedNamingTypedCollisionKeepsDiagnosticsAndViewNames(t *testing.T) {
	const hidden = "hidden-collision-origin"
	r, err := NewSharedUpstreams([]config.MCPServerEntry{
		{ID: "alpha", ToolPrefix: "alpha_", Transport: "http", AllowUnconfinedRemote: true},
		{ID: hidden, Transport: "http", AllowUnconfinedRemote: true},
		{ID: "gamma", Transport: "http", AllowUnconfinedRemote: true},
	}, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.pool.SetConnectFunc(func(ctx context.Context, entry config.MCPServerEntry) (upstreamClient, error) {
		name := "read"
		if entry.ID == hidden {
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, ok := r.registry.Lookup("alpha_read"); ok {
					break
				}
				if time.Now().After(deadline) {
					return nil, errors.New("alpha did not register")
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			name = "alpha_read"
		}
		if entry.ID == "gamma" {
			name = "gamma_read"
		}
		return &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool(name)}}, nil
		}, callToolFunc: func(_ context.Context, p *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			if p.Name != name {
				t.Errorf("forwarded final name %q instead of %q", p.Name, name)
			}
			return &mcpsdk.CallToolResult{}, nil
		}}, nil
	})
	var collision *mcpgateway.CollisionError
	if err := r.Start(context.Background()); !errors.As(err, &collision) {
		t.Fatalf("startup %v", err)
	}
	if err := r.Start(context.Background()); !errors.As(err, &collision) {
		t.Fatalf("repeat startup lost error: %v", err)
	}
	if len(r.pool.StatusSummary()) != 3 {
		t.Fatal("shared status disappeared")
	}
	if _, ok := r.registry.Lookup("gamma_read"); !ok {
		t.Fatal("healthy inventory disappeared")
	}
	if _, ok := r.registry.Lookup(hidden + "__alpha_read"); ok {
		t.Fatal("shared pool created alias")
	}
	for _, mode := range []mcpgateway.Mode{mcpgateway.Flat, mcpgateway.Search} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "caller", Kind: "service"})
			a, err := NewVerifiedAdapter(ctx, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			v, err := r.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{"alpha"}, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: string(mode), Source: "test"}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			cs := connectDaemonView(ctx, t, v)
			name, args := "alpha_read", map[string]any{}
			if mode == mcpgateway.Search {
				name, args = "tether_tool_call", map[string]any{"name": name, "arguments": args}
			}
			result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
			if err != nil || result.IsError {
				t.Fatalf("call %v %v", result, err)
			}
			raw, _ := json.Marshal(v.Service.Status(""))
			if strings.Contains(string(raw), hidden) {
				t.Fatalf("hidden owner leaked: %s", raw)
			}
			if v.Service.Status("").Complete {
				t.Fatal("degraded collision reported complete")
			}
			rt, err := v.Service.ResolveTarget("alpha_read")
			if err != nil || rt.Origin != "alpha" {
				t.Fatal("view names changed")
			}
		})
	}
}

func TestSharedNamingReconnectRebindsAcceptedToolsInEveryView(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("real protect-only upstream smoke requires Linux bubblewrap")
	}
	// As in the shared confinement smoke, probe the host independently of
	// product startup: CI AppArmor can deny namespaces even with bwrap installed.
	probe := exec.Command("bwrap", "--bind", "/", "/", "--unshare-user", "--unshare-pid", "--proc", "/proc", "/bin/true")
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("host cannot create bubblewrap namespace: %v (%s)", err, output)
	}
	dir := t.TempDir()
	alpha := fixtureEntry(t, dir, "alpha")
	alpha.Env["TETHER_UPSTREAM_CALL_INSTANCE"] = "1"
	r, err := NewSharedUpstreams([]config.MCPServerEntry{alpha, fixtureEntry(t, dir, "beta")}, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// The fixture's PID is namespace-local. Wait on actual process exit
		// before removing its files; Shutdown only signals EOF to children.
		r.pool.mu.Lock()
		var exits []<-chan struct{}
		for _, status := range r.pool.statuses {
			if leaf, ok := status.client.(*stdioUpstream); ok {
				exits = append(exits, leaf.done)
			}
		}
		r.pool.mu.Unlock()
		r.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, exited := range exits {
			select {
			case <-exited:
			case <-ctx.Done():
				t.Error("fixture process did not exit after EOF")
			}
		}
	}()
	r.pool.policy.Delays = []time.Duration{10 * time.Millisecond}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "caller", Kind: "service"})
	type caller struct {
		mode    mcpgateway.Mode
		client  *mcpsdk.ClientSession
		before  string
		service *mcpgateway.Service
	}
	callers := []caller{}
	call := func(cs *mcpsdk.ClientSession, mode mcpgateway.Mode) string {
		name, args := "alpha_probe", map[string]any{}
		if mode == mcpgateway.Search {
			name, args = "tether_tool_call", map[string]any{"name": name, "arguments": args}
		}
		result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("accepted view call %v %v", result, err)
		}
		return textOf(result)
	}
	for _, mode := range []mcpgateway.Mode{mcpgateway.Flat, mcpgateway.Search} {
		a, err := NewVerifiedAdapter(ctx, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{"alpha", "beta"}, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: string(mode), Source: "test"}}}})
		if err != nil {
			t.Fatal(err)
		}
		defer v.Close()
		cs := connectDaemonView(ctx, t, v)
		callers = append(callers, caller{mode: mode, client: cs, before: call(cs, mode), service: v.Service})
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha.tool"), []byte("beta_probe"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureMarker(t, dir, "alpha", "1")
	awaitStatus(t, r.pool, "alpha", func(s ServerStatus) bool { return s.Status == "connected" && s.Degraded })
	// NewGatewayView serializes registry/client publication with catalogMu.
	r.pool.catalogMu.Lock()
	_, collisions := r.registry.NameDiagnostics()
	r.pool.catalogMu.Unlock()
	if len(collisions) == 0 {
		t.Fatal("reconnect collision finding disappeared during view publication")
	}
	for _, c := range callers {
		if after := call(c.client, c.mode); after == c.before {
			t.Fatalf("shared %s view retained old process identity: before=%q after=%q", c.mode, c.before, after)
		}
		status := c.service.Status("")
		if status.Complete || len(status.Collisions) == 0 {
			t.Fatalf("rejected refresh diagnostic lost %+v", status)
		}
	}
}

func TestSharedNamingSchemaRefreshKeepsAcceptedToolsCallable(t *testing.T) {
	r, err := NewSharedUpstreams([]config.MCPServerEntry{{ID: "alpha", Transport: "http", AllowUnconfinedRemote: true}}, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var version atomic.Int64
	r.pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
		return &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			def := makeTool("alpha_probe")
			def.Description = fmt.Sprintf("version %d", version.Add(1))
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{def}}, nil
		}, callToolFunc: func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{}, nil
		}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ctx = identity.WithPrincipal(ctx, identity.Principal{ID: "caller", Kind: "service"})
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, mode := range []mcpgateway.Mode{mcpgateway.Flat, mcpgateway.Search} {
		a, err := NewVerifiedAdapter(ctx, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		v, err := r.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{"alpha"}, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: string(mode), Source: "test"}}}})
		if err != nil {
			t.Fatal(err)
		}
		defer v.Close()
		cs := connectDaemonView(ctx, t, v)
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, args := "alpha_probe", map[string]any{}
			if mode == mcpgateway.Search {
				name, args = "tether_tool_call", map[string]any{"name": name, "arguments": args}
			}
			for range 1000 {
				result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
				if err != nil || result.IsError {
					t.Errorf("accepted %s view call during refresh: %v %v", mode, result, err)
					return
				}
			}
		}()
	}
	for range 1000 {
		if _, err := r.pool.RefreshServer(ctx, "alpha"); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
