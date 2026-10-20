package mcpadapter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Every tool a client is handed by a daemon view must parse in a strict MCP
// client: valid name, object schema, no null keywords. This checks the generated
// list itself, not a count, so a new native tool or an upstream shape that breaks
// clients (a null "required", a dotted name) fails here.
func TestDaemonViewToolListIsValidForStrictClients(t *testing.T) {
	r, err := NewSharedUpstreams(nil, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "native.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, mode := range []mcpgateway.Mode{mcpgateway.Flat, mcpgateway.Search} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "restricted", Kind: "service"})
			a, err := NewVerifiedAdapter(ctx, &app.Service{Catalog: &config.Catalog{}, Store: db}, nil)
			if err != nil {
				t.Fatal(err)
			}
			v, err := r.NewGatewayView(ctx, a, ProxyOptions{ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: string(mode), Source: "test"}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			cs := connectDaemonView(ctx, t, v)
			listed, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(listed.Tools) == 0 {
				t.Fatal("no tools listed")
			}
			for _, tool := range listed.Tools {
				if problems := mcpgateway.ValidateToolForClient(tool); len(problems) != 0 {
					t.Errorf("%s: %v", tool.Name, problems)
				}
			}
		})
	}
}

// tether_gateway_status answers compactly by default and pages lint only on request.
func TestGatewayStatusToolDefaultsToCompactSummary(t *testing.T) {
	r, err := NewSharedUpstreams(nil, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "native.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "restricted", Kind: "service"})
	a, err := NewVerifiedAdapter(ctx, &app.Service{Catalog: &config.Catalog{}, Store: db}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.NewGatewayView(ctx, a, ProxyOptions{ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: "search", Source: "test"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	cs := connectDaemonView(ctx, t, v)
	call := func(args map[string]any) (map[string]any, string) {
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_gateway_status", Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("status %v: err=%v res=%+v", args, err, res)
		}
		text := res.Content[0].(*mcpsdk.TextContent).Text
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(err)
		}
		return out, text
	}
	summary, text := call(map[string]any{})
	if _, ok := summary["explanation"]; !ok || summary["lint_by_code"] == nil {
		t.Fatalf("default is not the compact summary: %s", text)
	}
	if _, hasLint := summary["lint"]; hasLint || len(text) > 8192 {
		t.Fatalf("summary carries lint detail or is too big (%d bytes)", len(text))
	}
	full, _ := call(map[string]any{"detail": "full", "limit": 1})
	if _, ok := full["lint_total"]; !ok || full["lint"] == nil {
		t.Fatalf("detail=full lacks paged lint: %v", full)
	}
	if res, _ := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_gateway_status", Arguments: map[string]any{"limit": 5}}); res == nil || !res.IsError {
		t.Fatal("limit without detail=full accepted")
	}
}
