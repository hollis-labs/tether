package mcptransport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
)

func TestTelemetryHTTPKeepsPresentedCredentialIdentity(t *testing.T) {
	f := newTransportFixture(t, false, false)
	if err := f.db.CreateSession(store.SessionRow{ID: "actual", LogicalAgentID: "agent", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SaveSessionMCPPolicy(context.Background(), mcpgateway.SessionPolicy{SessionID: "actual", AgentID: "agent", Servers: []string{}}.Seal()); err != nil {
		t.Fatal(err)
	}
	for _, principal := range []identity.Principal{{ID: "session:actual", Kind: "session", SessionID: "actual"}, {ID: identity.OperatorID, Kind: "operator"}, {ID: "service", Kind: "service"}, {}} {
		t.Run(principal.Kind, func(t *testing.T) {
			token := ""
			if principal.Kind == "service" {
				token = f.token(t, principal.ID, nil)
			} else if principal.ID != "" {
				var err error
				token, err = f.ids.Mint(context.Background(), principal)
				if err != nil {
					t.Fatal(err)
				}
			}
			response := f.request(t, http.MethodPost, "/mcp", token, "", nil)
			session := response.Header.Get("Mcp-Session-Id")
			status := response.StatusCode
			closeResponse(response)
			if principal.ID == "" {
				if status != http.StatusUnauthorized {
					t.Fatalf("anonymous MCP admission = %d", status)
				}
				// Strict MCP admission refuses anonymous calls before observation.
				return
			}
			if status != http.StatusOK || session == "" {
				t.Fatalf("credential admission: %d / %q", status, session)
			}
			tool := "missing_" + principal.Kind
			body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": map[string]any{}, "_meta": map[string]any{"tether.context": map[string]any{"principal_id": "forged", "session_id": "forged", "verified": true}}}})
			req, err := http.NewRequest(http.MethodPost, daemon.BaseURL(f.addr)+"/mcp", strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Mcp-Session-Id", session)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			response, err = daemon.DialHTTPClient(f.addr).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			closeResponse(response)
			want := callcontext.Snapshot{Source: "daemon", PrincipalID: principal.ID, PrincipalKind: principal.Kind}
			if principal.Kind == "session" {
				want.Verified, want.SessionID, want.LogicalAgentID = true, principal.SessionID, "agent"
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				rows, err := f.db.EventsSince(0)
				if err != nil {
					t.Fatal(err)
				}
				var start, end bool
				for _, row := range rows {
					if row.Kind != events.EventTypeToolCallStart && row.Kind != events.EventTypeToolCallEnd {
						continue
					}
					var call events.ToolCallEvent
					if err := json.Unmarshal([]byte(row.PayloadJSON), &call); err != nil {
						t.Fatal(err)
					}
					if call.ToolName != tool {
						continue
					}
					if call.Attribution != want {
						t.Fatalf("MCP credential attribution = %+v, want %+v", call.Attribution, want)
					}
					start = start || row.Kind == events.EventTypeToolCallStart
					end = end || row.Kind == events.EventTypeToolCallEnd
				}
				if start && end {
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("credentialed call not recorded")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
	rows, err := f.db.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Kind == events.EventTypeToolCallStart || row.Kind == events.EventTypeToolCallEnd {
			var call events.ToolCallEvent
			_ = json.Unmarshal([]byte(row.PayloadJSON), &call)
			if call.Attribution.PrincipalID == "" {
				t.Fatal("anonymous MCP call bypassed strict admission")
			}
		}
	}
}
