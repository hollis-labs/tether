package mcptransport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
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
			want := callcontext.Snapshot{Verified: true, Source: "daemon", PrincipalID: principal.ID, PrincipalKind: principal.Kind}
			if principal.Kind == "session" {
				want.SessionResolved, want.SessionID, want.LogicalAgentID, want.Intent = true, principal.SessionID, "agent", "fresh"
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				// The event log carries the canonical pair; the completed call's
				// attribution is in proxy_events (CW-20261113-0009).
				rows, err := f.db.EventsSince(0)
				if err != nil {
					t.Fatal(err)
				}
				var start, end bool
				for _, row := range rows {
					if row.Kind == events.EventTypeToolCallStart || row.Kind == events.EventTypeToolCallEnd {
						t.Fatalf("legacy %s logged", row.Kind)
					}
					if row.Kind != catalog.KindToolCallStarted && row.Kind != catalog.KindToolCallFinished {
						continue
					}
					var payload struct {
						Tool string `json:"tool"`
					}
					if err := json.Unmarshal([]byte(row.PayloadJSON), &payload); err != nil {
						t.Fatal(err)
					}
					if payload.Tool != tool {
						continue
					}
					start = start || row.Kind == catalog.KindToolCallStarted
					end = end || row.Kind == catalog.KindToolCallFinished
				}
				proxied, err := f.db.QueryProxyEvents(store.ProxyEventFilter{ToolName: tool})
				if err != nil {
					t.Fatal(err)
				}
				if len(proxied) > 0 && proxied[0].Attribution != want {
					t.Fatalf("MCP credential attribution = %+v, want %+v", proxied[0].Attribution, want)
				}
				if start && end && len(proxied) > 0 {
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("credentialed call not recorded")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
	proxied, err := f.db.QueryProxyEvents(store.ProxyEventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range proxied {
		if row.Attribution.PrincipalID == "" {
			t.Fatal("anonymous MCP call bypassed strict admission")
		}
	}
}
