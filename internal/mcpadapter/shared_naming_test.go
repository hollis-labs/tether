package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	dir := t.TempDir()
	alpha := fixtureEntry(t, dir, "alpha")
	alpha.Env["TETHER_UPSTREAM_CALL_INSTANCE"] = "1"
	r, err := NewSharedUpstreams([]config.MCPServerEntry{alpha, fixtureEntry(t, dir, "beta")}, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
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
