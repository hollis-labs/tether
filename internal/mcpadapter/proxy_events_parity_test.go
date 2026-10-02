package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type parityProxyRows struct {
	rows []store.ProxyEvent
	err  error
}

func (s parityProxyRows) AppendProxyEvent(store.ProxyEvent) error { return nil }
func (s parityProxyRows) QueryProxyEvents(store.ProxyEventFilter) ([]store.ProxyEvent, error) {
	return s.rows, s.err
}

// Existing HTTP/client and both native MCP tools retain their different wire
// shapes while exposing the same normalized records and error codes.
func TestProxyEvents_HTTPClientMCPParity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CODEX_") || strings.HasPrefix(key, "TETHER_") {
			t.Setenv(key, "")
		}
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			original := store.ProxyEvent{ID: 7, SessionID: "own", Server: "upstream", ToolName: "tool", ArgsSchemaFP: "fp", DurationMs: 19, OK: false, Error: "upstream failure", Timestamp: time.Date(2026, 10, 2, 1, 2, 3, 123, time.UTC), Attribution: callcontext.Snapshot{Verified: true, PrincipalID: "session:own", SessionID: "own", Source: "daemon"}, ClaimedSessionID: "other"}
			st := parityProxyRows{rows: []store.ProxyEvent{original}}
			if fail {
				st.err = errors.New("query unavailable")
			}
			h := api.NewHandler(api.Deps{ProxyEvents: st})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/proxy/events?limit=50", nil))
			var httpBody struct {
				Events []api.ProxyEventDTO
				Count  int
				Error  struct{ Code string }
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &httpBody); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(h)
			defer srv.Close()
			dc := client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(""))
			clientRows, clientErr := dc.ProxyEvents(context.Background(), client.ProxyEventsQuery{Limit: 50})
			a := NewWithDaemon(&app.Service{}, dc, "", nil)
			s := gomcp.NewServer("parity", "test")
			a.registerObservationTools(s)
			a.registerToolCallEventsTool(s, DaemonProxyEvents{Client: dc})
			c := connectInMemory(t, s)
			for _, name := range []string{"tether_proxy_events", "tether_events_tool_calls"} {
				res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: map[string]any{"limit": 50}})
				if err != nil {
					t.Fatal(err)
				}
				var body struct {
					Events []store.ProxyEvent
					Count  int
					Code   string
				}
				if err := json.Unmarshal([]byte(textOf(res)), &body); err != nil {
					t.Fatal(err)
				}
				if fail {
					if rr.Code != http.StatusInternalServerError || httpBody.Error.Code != "internal_error" || !res.IsError || body.Code != httpBody.Error.Code || clientErr == nil || !strings.Contains(clientErr.Error(), "(internal_error)") {
						t.Fatalf("%s error parity HTTP=%d/%s client=%v MCP=%s", name, rr.Code, httpBody.Error.Code, clientErr, textOf(res))
					}
					continue
				}
				wantHTTP := api.ProxyEventDTO{ID: original.ID, Attribution: original.Attribution, ClaimedSessionID: original.ClaimedSessionID, SessionID: original.SessionID, Server: original.Server, ToolName: original.ToolName, ArgsSchemaFP: original.ArgsSchemaFP, DurationMs: original.DurationMs, OK: original.OK, Error: original.Error, Timestamp: original.Timestamp.Format(time.RFC3339Nano)}
				if rr.Code != http.StatusOK || res.IsError || clientErr != nil || httpBody.Count != 1 || body.Count != 1 || !reflect.DeepEqual(httpBody.Events, []api.ProxyEventDTO{wantHTTP}) || !reflect.DeepEqual(clientRows, httpBody.Events) || !reflect.DeepEqual(body.Events, st.rows) {
					t.Fatalf("%s parity HTTP=%+v client=%+v/%v MCP=%s", name, httpBody, clientRows, clientErr, textOf(res))
				}
				// Compare actual MCP event JSON with its original storage-shaped contract;
				// the application must not silently replace it with the HTTP DTO shape.
				var wire struct{ Events json.RawMessage }
				_ = json.Unmarshal([]byte(textOf(res)), &wire)
				want, _ := json.Marshal(st.rows)
				var gotValue, wantValue any
				_ = json.Unmarshal(wire.Events, &gotValue)
				_ = json.Unmarshal(want, &wantValue)
				if !reflect.DeepEqual(gotValue, wantValue) {
					t.Fatalf("%s changed wire shape: %s want %s", name, wire.Events, want)
				}
			}
		})
	}
}
