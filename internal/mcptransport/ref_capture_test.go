package mcptransport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpforward"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The thin relay crosses the real /mcp admission/view boundary and invokes a
// real upstream fixture. Only the hub's observed successful traffic may write
// proxy refs; the requesting process has no proxy credential or write channel.
func TestHubCapturesForwardedRefsWithCredentialSession(t *testing.T) {
	for _, mode := range []string{"flat", "search"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", mode, enabled), func(t *testing.T) {
				f := newTransportFixture(t, true, true)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				const id = "actual-session"
				if err := f.db.CreateSession(store.SessionRow{ID: id, LogicalAgentID: "agent", State: "running"}, &launch.Plan{}); err != nil {
					t.Fatal(err)
				}
				if err := f.db.SaveSessionMCPPolicy(ctx, mcpgateway.SessionPolicy{SessionID: id, AgentID: "agent", Servers: []string{"app"}, DiscoveryMode: &mode, ExtractRefs: enabled}.Seal()); err != nil {
					t.Fatal(err)
				}
				token, err := f.ids.Mint(ctx, identity.Principal{ID: "session:" + id, Kind: "session", SessionID: id})
				if err != nil {
					t.Fatal(err)
				}
				left, right := mcpsdk.NewInMemoryTransports()
				done := make(chan error, 1)
				go func() {
					done <- mcpforward.Run(ctx, client.New(f.addr, client.WithToken(token)), client.MCPOptions{}, right)
				}()
				session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "capture-test", Version: "1"}, nil).Connect(ctx, left, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					_ = session.Close()
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("forwarder did not stop")
					}
				}()
				call := func(ref string) (*mcpsdk.CallToolResult, error) {
					name := "app_echo"
					args := map[string]any{"message": ref}
					if mode == "search" {
						name = "tether_tool_call"
						args = map[string]any{"name": "app_echo", "arguments": args}
					}
					return session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args, Meta: mcpsdk.Meta{"Tether.Context": map[string]any{"session_id": "forged"}, "tether.provenance": map[string]any{"session_id": "forged", "source": "proxy"}}})
				}
				result, err := call("CW-20261016-9999")
				if err == nil && !result.IsError {
					t.Fatal("fixture failure unexpectedly succeeded")
				}
				for range 2 {
					result, err = call("CW-20261016-0157")
					if err != nil || result.IsError {
						t.Fatalf("relayed tool failed: %v", err)
					}
				}
				refs, err := f.db.ListSessionRefs(id, store.ListSessionRefsOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if enabled {
					if len(refs) != 1 || refs[0].RefID != "CW-20261016-0157" || refs[0].Source != store.SourceProxy || refs[0].Relation != store.RelationReferenced || refs[0].Kind != store.KindTorqueTask {
						t.Fatalf("hub capture=%+v", refs)
					}
				} else if len(refs) != 0 {
					t.Fatalf("disabled policy captured=%+v", refs)
				}
				forged, err := f.db.ListSessionRefs("forged", store.ListSessionRefsOptions{})
				if err != nil || len(forged) != 0 {
					t.Fatalf("forged attribution=%+v err=%v", forged, err)
				}
				if err := f.ids.RevokeSession(ctx, id); err != nil {
					t.Fatal(err)
				}
				result, err = call("CW-20261016-0158")
				if err == nil && !result.IsError {
					t.Fatal("revoked session forwarded a call")
				}
				after, err := f.db.ListSessionRefs(id, store.ListSessionRefsOptions{})
				if err != nil || len(after) != len(refs) {
					t.Fatalf("revoked call captured=%+v err=%v", after, err)
				}
			})
		}
	}
}
