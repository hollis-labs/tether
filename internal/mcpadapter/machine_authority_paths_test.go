package mcpadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A read-only agent is refused a write tool on a machine-changing upstream
// through both gateway paths: the per-agent proxy and the daemon-owned view.
func TestMachineAuthority_ReadOnlyRefusedWriteToolOnBothPaths(t *testing.T) {
	t.Run("proxy", func(t *testing.T) {
		catalog, _ := proxyCatalog(t, "cerberus")
		cs := connectProxyMode(t, catalog, "search", false, "cerberus")
		text, isErr := tetherCall(t, cs, "cerberus_probe")
		if !isErr || !strings.Contains(text, "machine authority") {
			t.Fatalf("write tool not refused: %v %s", isErr, text)
		}
	})
	t.Run("shared view", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		for _, tc := range []struct {
			access  string
			refused bool
		}{{"", true}, {"read-only", true}, {"read-write", false}} {
			t.Run("access="+tc.access, func(t *testing.T) {
				r, err := NewSharedUpstreams([]config.MCPServerEntry{{ID: "cerberus", Transport: "http"}}, daemonTestRoots(t), true)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				if err := r.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				dispatched := false
				s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "cerberus", Version: "test"}, nil)
				s.AddTool(&mcpsdk.Tool{Name: "cerberus_probe", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
					dispatched = true
					return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
				})
				st, ct := mcpsdk.NewInMemoryTransports()
				ss, err := s.Connect(context.Background(), st, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ss.Close() })
				up, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "fixture", Version: "test"}, nil).Connect(context.Background(), ct, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = up.Close() })
				tools, err := up.ListTools(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				r.pool.mu.Lock()
				r.pool.statuses["cerberus"].client = up
				r.pool.mu.Unlock()
				if _, err := r.pool.publish(context.Background(), "cerberus", up, tools.Tools, false); err != nil {
					t.Fatal(err)
				}
				ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "caller", Kind: "session", SessionID: "actual", Scopes: []string{"message.write"}})
				a, err := NewVerifiedAdapter(ctx, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				v, err := r.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{"cerberus"}, MachineAccess: tc.access, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: "flat", Source: "test"}}}})
				if err != nil {
					t.Fatal(err)
				}
				defer v.Close()
				cs := connectDaemonView(ctx, t, v)
				res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "cerberus_probe", Arguments: map[string]any{}})
				refused := err != nil || res.IsError
				if refused != tc.refused || dispatched == tc.refused {
					t.Fatalf("access=%q refused=%v dispatched=%v res=%+v err=%v", tc.access, refused, dispatched, res, err)
				}
			})
		}
	})
}
