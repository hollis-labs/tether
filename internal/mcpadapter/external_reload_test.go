package mcpadapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestExternalReloadUpdatesExistingGatewayHealth(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("started=%v", started), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "mcp-servers"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			write := func(external bool) {
				t.Helper()
				body := "id: changing\ntransport: stdio\ncommand: unused\n"
				if external {
					body += "served_by: external\n"
				}
				if err := os.WriteFile(filepath.Join(root, "mcp-servers/changing.yaml"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(false)
			cat, err := config.LoadLayered(root)
			if err != nil {
				t.Fatal(err)
			}
			svc := &app.Service{CatalogRoot: root, Catalog: cat}
			entries := []config.MCPServerEntry{}
			if started {
				entries = append(entries, config.MCPServerEntry{ID: "changing", Transport: "stdio", Command: "unused"})
			}
			runtime, err := NewSharedUpstreams(entries, daemonTestRoots(t), true)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			runtime.pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
				return nil, fmt.Errorf("deliberately unavailable")
			})
			if err := runtime.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if started {
				if err := runtime.registry.Register("changing", nil, []*mcpsdk.Tool{makeTool("changing_probe")}); err != nil {
					t.Fatal(err)
				}
			}
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "reload-reader", Kind: "service"})
			a, err := NewVerifiedAdapter(ctx, svc, nil)
			if err != nil {
				t.Fatal(err)
			}
			view, err := runtime.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{"changing"}})
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
			if origins := view.Service.Snapshot().Origins; len(origins) != 1 {
				t.Fatalf("initial origins: %+v", origins)
			}
			if started && len(view.Service.Snapshot().Entries) != 1 {
				t.Fatal("initial inventory missing")
			}
			if started && parseToolJSON(t, callHandler(t, a.handleHealth, nil))["ok"] != false {
				t.Fatal("failed internal origin must fail health")
			}
			write(true)
			report, err := svc.ReloadCatalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, warning := range report.RestartRequired {
				if strings.Contains(warning, "changing") {
					t.Fatal(warning)
				}
			}
			if origins := view.Service.Snapshot().Origins; len(origins) != 0 {
				t.Fatalf("existing view retained external origin: %+v", origins)
			}
			if entries := view.Service.Snapshot().Entries; len(entries) != 0 {
				t.Fatalf("existing view retained external inventory: %+v", entries)
			}
			if statuses := a.upstreams.StatusSummary(); len(statuses) != 0 {
				t.Fatalf("external health status: %+v", statuses)
			}
			if health := parseToolJSON(t, callHandler(t, a.handleHealth, nil)); health["ok"] != true {
				t.Fatalf("health after external reload: %v", health)
			}
			write(false)
			report, err = svc.ReloadCatalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, warning := range report.RestartRequired {
				found = found || strings.Contains(warning, "changing")
			}
			if !found {
				t.Fatal("external to built-in must report restart")
			}
			if origins := view.Service.Snapshot().Origins; len(origins) != 1 {
				t.Fatalf("existing view after return to built-in: %+v", origins)
			}
			if started && len(view.Service.Snapshot().Entries) != 1 {
				t.Fatal("returning internal inventory missing")
			}
			if started && parseToolJSON(t, callHandler(t, a.handleHealth, nil))["ok"] != false {
				t.Fatal("returning internal failure must affect health again")
			}
		})
	}
}

func TestExternalGatewayPreservesKnownDisabledRefusal(t *testing.T) {
	for _, external := range []bool{false, true} {
		for _, catalogEnabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("external=%v/catalogEnabled=%v", external, catalogEnabled), func(t *testing.T) {
				disabled := false
				entry := config.MCPServerEntry{ID: "disabled", Enabled: &disabled}
				if external {
					entry.ServedBy = "external"
				}
				runtime, err := NewSharedUpstreams([]config.MCPServerEntry{entry}, daemonTestRoots(t), true)
				if err != nil {
					t.Fatal(err)
				}
				defer runtime.Close()
				ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "disabled-reader", Kind: "service"})
				a, err := NewVerifiedAdapter(ctx, &app.Service{Catalog: &config.Catalog{MCPServerEnabled: map[string]bool{entry.ID: catalogEnabled}, MCPServerExternal: map[string]bool{entry.ID: external}}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				view, err := runtime.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{entry.ID}})
				if err == nil {
					view.Close()
					t.Fatal("known-disabled origin accepted")
				}
			})
		}
	}
}
