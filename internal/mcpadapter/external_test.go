package mcpadapter

import (
	"context"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"testing"
)

func TestExternalMCPNeverConnectsOrDegradesGateway(t *testing.T) {
	ctx := context.Background()
	entry := config.MCPServerEntry{ID: "shared-services", ServedBy: "external", Transport: "stdio", Command: "must-not-run"}
	pool := NewClientPool([]config.MCPServerEntry{entry}, NewToolRegistry())
	pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
		t.Error("external connection attempted")
		return nil, nil
	})
	if err := pool.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	if len(pool.StatusSummary()) != 0 {
		t.Fatal(pool.StatusSummary())
	}
	for _, afterReload := range []bool{false, true} {
		entries := []config.MCPServerEntry{entry}
		if afterReload {
			entries = nil
		}
		runtime, err := NewSharedUpstreams(entries, daemonTestRoots(t), true)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		if err := runtime.Start(ctx); err != nil {
			t.Fatal(err)
		}
		ctx := identity.WithPrincipal(ctx, identity.Principal{Verified: true, ID: "external-reader", Kind: "service"})
		a, err := NewVerifiedAdapter(ctx, &app.Service{Catalog: &config.Catalog{MCPServerEnabled: map[string]bool{entry.ID: true}, MCPServerExternal: map[string]bool{entry.ID: true}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		view, err := runtime.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{entry.ID}, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: "flat", Source: "test"}}}})
		if err != nil {
			t.Fatal(err)
		}
		snapshot := view.Service.Snapshot()
		view.Close()
		if len(snapshot.Origins) != 0 {
			t.Fatalf("external status/pending_restart leaked (reload=%v): %+v", afterReload, snapshot.Origins)
		}
		health := parseToolJSON(t, callHandler(t, a.handleHealth, nil))
		if health["ok"] != true {
			t.Fatalf("external origin failed health: %v", health)
		}
		if len(a.upstreams.StatusSummary()) != 0 {
			t.Fatal("external upstream affects health")
		}

		// Switching ownership back to the built-in gateway needs a restart.
		a.svc.Catalog = &config.Catalog{MCPServerEnabled: map[string]bool{entry.ID: true}}
		pending, err := runtime.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{entry.ID}})
		if err != nil {
			t.Fatal(err)
		}
		origins := pending.Service.Snapshot().Origins
		pending.Close()
		if len(origins) != 1 || origins[0].Status != "pending_restart" {
			t.Fatalf("ownership transition: %+v", origins)
		}
	}
}
