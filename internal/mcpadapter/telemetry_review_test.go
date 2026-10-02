package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app/proxyevents"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTelemetryStdioKeepsPresentedCredentialIdentity(t *testing.T) {
	f := newDaemonOnlyFixture(t)
	ids := identity.NewStore(f.db.DB())
	server := &daemon.Server{Config: daemon.Config{IdentityMode: identity.Observe}, Identity: ids, Service: f.service}
	defer server.CloseIdentityAudit()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	for _, principal := range []identity.Principal{{ID: "session:sess-1", Kind: "session", SessionID: "sess-1"}, {ID: identity.OperatorID, Kind: "operator"}, {ID: "service", Kind: "service"}, {}} {
		t.Run(principal.Kind, func(t *testing.T) {
			token := ""
			if principal.ID != "" {
				var err error
				token, err = ids.Mint(context.Background(), principal)
				if err != nil {
					t.Fatal(err)
				}
			}
			dc := client.New("tcp:"+strings.TrimPrefix(httpServer.URL, "http://"), client.WithToken(token))
			a := NewWithDaemon(nil, dc, "marker", nil)
			a.SessionID = "self-asserted-session"
			capture := &callCapturePublisher{}
			logging := NewLoggingMiddleware(capture)
			logging.contextDecorator = a.withSessionID
			sdk := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "stdio-observation", Version: "test"}, nil)
			sdk.AddTool(&mcpsdk.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{}, nil
			})
			sdk.AddReceivingMiddleware(proxyLoggingMiddleware([]ToolCallMiddleware{logging}))
			cs := connectDaemonView(context.Background(), t, &GatewayView{Server: sdk})
			_, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "read", Meta: mcpsdk.Meta{ContextMetaKey: map[string]any{"principal_id": "forged", "agent_urn": "forged", "workstream_id": "forged", "verified": true}}})
			if err != nil {
				t.Fatal(err)
			}
			want, err := dc.CallerContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if principal.ID == "" && want != (callcontext.Snapshot{}) {
				t.Fatal("anonymous caller acquired identity", want)
			}
			if want.PrincipalID != principal.ID || want.Verified != (principal.Kind == "session") {
				t.Fatal("fixture credential resolution", want)
			}
			if len(capture.calls) != 2 {
				t.Fatalf("expected start and end, got %d", len(capture.calls))
			}
			for _, call := range capture.calls {
				if call.Attribution != want || call.ClaimedSessionID != "self-asserted-session" {
					t.Fatalf("stdio credential/claim parity: %+v / %+v", call, want)
				}
			}
		})
	}
}

type reviewIngestPublisher struct {
	client   *client.Client
	failures []error
}

func (publisher *reviewIngestPublisher) Publish(ctx context.Context, event events.Event) error {
	var call events.ToolCallEvent
	if err := json.Unmarshal([]byte(event.PayloadJSON), &call); err != nil {
		return err
	}
	phase := proxyevents.ProxyEventPhaseEnd
	if event.Kind == events.EventTypeToolCallStart {
		phase = proxyevents.ProxyEventPhaseStart
	}
	err := publisher.client.IngestProxyEvent(ctx, api.ProxyEventIngestRequest(proxyevents.IngestCall(call, phase, true)))
	if err != nil {
		publisher.failures = append(publisher.failures, err)
	}
	return err
}

func TestTelemetryLargeDeniedNamesSurviveCredentialedIngest(t *testing.T) {
	f := newDaemonOnlyFixture(t)
	server := &daemon.Server{Service: f.service, ProxyEvents: f.db, Bus: events.NewBus(events.BusOptions{Persister: f.db})}
	defer server.CloseIdentityAudit()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	dc := client.New("tcp:"+strings.TrimPrefix(httpServer.URL, "http://"), client.WithToken(""))
	for _, size := range []int{1 << 20, 8 << 20} {
		publisher := &reviewIngestPublisher{client: dc}
		_, _ = NewLoggingMiddleware(publisher).Handle(context.Background(), ToolCall{ToolName: strings.Repeat("x", size)}, NewProxyRouter(NewToolRegistry()).Handle)
		if len(publisher.failures) != 0 {
			t.Fatal("bounded denied call rejected by ingest", publisher.failures)
		}
	}
	rows, err := f.db.QueryProxyEvents(store.ProxyEventFilter{ServerID: "tether"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("denied calls missing: %+v %v", rows, err)
	}
	for _, row := range rows {
		if len(row.ToolName) > 256 || row.ErrorClass != events.ToolErrorDenied {
			t.Fatalf("unbounded or untyped call: %+v", row)
		}
	}
}
