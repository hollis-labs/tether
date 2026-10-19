package mcpadapter

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
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
