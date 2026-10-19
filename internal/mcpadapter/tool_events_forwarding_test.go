package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/eventstamp"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Exercise the actual async sender, credentialed HTTP boundary, validated legacy
// ingest and stamped persistent bus. Same-named overlapping calls require their
// original trace/span identity to pair phases; daemon request spans cannot do it.
func TestCanonicalToolEventsConcurrentDaemonOnlyForwarding(t *testing.T) {
	previous := otel.GetTracerProvider()
	provider := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(provider)
	defer func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) }()
	for _, bound := range []bool{true, false} {
		name := "operator"
		if bound {
			name = "session"
		}
		t.Run(name, func(t *testing.T) {
			f := newDaemonOnlyFixture(t)
			if _, err := f.db.DB().Exec("UPDATE sessions SET task_id = ? WHERE id = ?", "TASK-STORED", "sess-1"); err != nil {
				t.Fatal(err)
			}
			reg := registry.NewService(registry.NewStorage(f.db.DB()))
			agent := registry.LogicalAgentBindingTarget("worker")
			if _, err := reg.LeaseBinding(context.Background(), agent, "sess-1", "local", "sess-1", nil, registry.VisibilityPrivateLocal, time.Hour); err != nil {
				t.Fatal(err)
			}
			ids := identity.NewStore(f.db.DB())
			principal := identity.Principal{ID: identity.OperatorID, Kind: "operator"}
			if bound {
				principal = identity.Principal{ID: "session:sess-1", Kind: "session", SessionID: "sess-1"}
			}
			token, err := ids.Mint(context.Background(), principal)
			if err != nil {
				t.Fatal(err)
			}
			server := &daemon.Server{Config: daemon.Config{IdentityMode: identity.Observe}, Identity: ids, Service: f.service, Registry: reg, ProxyEvents: f.db, Bus: events.NewBus(events.BusOptions{Persister: f.db, Stages: []events.Stage{eventstamp.New()}})}
			defer server.CloseIdentityAudit()
			httpServer := httptest.NewServer(server.Handler())
			defer httpServer.Close()
			dc := client.New("tcp:"+strings.TrimPrefix(httpServer.URL, "http://"), client.WithToken(token))
			root, cancel := context.WithCancel(context.Background())
			defer cancel()
			sender := NewDaemonToolCallPublisher(root, dc)
			logging := NewLoggingMiddleware(sender)
			started := make(chan struct{}, 3)
			release := make(chan struct{})
			type expected struct {
				trace, span string
				ok          bool
				class       string
			}
			expectedCalls := make(chan expected, 3)
			var workers sync.WaitGroup
			for _, outcome := range []string{"success", "result_error", "timeout"} {
				workers.Add(1)
				go func() {
					defer workers.Done()
					// These are deliberately hostile local/body claims. Only the credential
					// and daemon session row may supply canonical identity and dimensions.
					ctx := callcontext.WithSnapshot(WithSessionID(root, "sess-1"), callcontext.Snapshot{Verified: true, SessionResolved: true, SessionID: "sess-1", TaskID: "FORGED-TASK", ProjectID: "FORGED-PROJECT", AgentURN: "FORGED-AGENT"})
					_, _ = logging.Handle(ctx, ToolCall{ToolName: "concurrent_read", Args: map[string]any{"password": "ARGUMENT-CANARY"}}, func(callCtx context.Context, _ ToolCall) (*mcpsdk.CallToolResult, error) {
						span := trace.SpanContextFromContext(callCtx)
						want := expected{trace: span.TraceID().String(), span: span.SpanID().String(), ok: outcome == "success"}
						if outcome == "result_error" {
							want.class = "upstream_error"
						}
						if outcome == "timeout" {
							want.class = "timeout"
						}
						expectedCalls <- want
						started <- struct{}{}
						<-release
						switch outcome {
						case "result_error":
							return errorResult("RESULT-CANARY"), nil
						case "timeout":
							return nil, context.DeadlineExceeded
						default:
							return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "RESULT-CANARY"}}}, nil
						}
					})
				}()
			}
			for range 3 {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("calls failed to overlap")
				}
			}
			close(release)
			workers.Wait()
			close(expectedCalls)
			wants := map[string]expected{}
			for want := range expectedCalls {
				wants[want.trace] = want
			}
			if len(wants) != 3 {
				t.Fatal("expected three distinct call traces")
			}
			var observed []events.Event
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				all, err := f.db.EventsSince(0)
				if err != nil {
					t.Fatal(err)
				}
				observed = nil
				for _, ev := range all {
					switch ev.Kind {
					case events.EventTypeToolCallStart, events.EventTypeToolCallEnd, catalog.KindToolCallStarted, catalog.KindToolCallFinished:
						observed = append(observed, ev)
					}
				}
				if len(observed) == 12 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if len(observed) != 12 {
				t.Fatalf("got %d observations; want 3 canonical and 3 legacy pairs", len(observed))
			}
			phases := map[string]map[string]int{}
			for _, ev := range observed {
				if ev.Kind != catalog.KindToolCallStarted && ev.Kind != catalog.KindToolCallFinished {
					continue
				}
				corr := ev.Envelope.Corr
				want, ok := wants[corr.TraceID]
				if !ok || corr.SpanID != want.span {
					t.Fatalf("forwarding lost original call correlation: %+v", corr)
				}
				if phases[corr.TraceID] == nil {
					phases[corr.TraceID] = map[string]int{}
				}
				phases[corr.TraceID][ev.Kind]++
				if bound {
					if ev.SessionID != "sess-1" || ev.Envelope.Dims.TaskID != "TASK-STORED" || ev.Envelope.Dims.ProjectID != "proj" || ev.Envelope.Dims.AgentURN != agent || ev.Envelope.Actor.URN != agent || ev.Envelope.Actor.Verified != "token" {
						t.Fatalf("credential scope lost or replaced by claim: %+v", ev.Envelope)
					}
				} else if ev.SessionID != "" || ev.Scope != events.ScopeDaemon || ev.Envelope.Dims != (events.ScopeDims{}) || ev.Envelope.Actor.URN != identity.OperatorID {
					t.Fatalf("operator body claimed a session: %+v", ev)
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
					t.Fatal(err)
				}
				for _, canary := range []string{"ARGUMENT-CANARY", "RESULT-CANARY", "FORGED-"} {
					if strings.Contains(ev.PayloadJSON, canary) {
						t.Fatal("canonical content/claim leak")
					}
				}
				for _, key := range []string{"args", "result", "error", "result_summary", "attribution"} {
					if _, ok := payload[key]; ok {
						t.Fatalf("content field %s", key)
					}
				}
				if ev.Kind == catalog.KindToolCallStarted {
					if _, ok := payload["ok"]; ok {
						t.Fatal("outcome at start")
					}
				} else if payload["ok"] != want.ok || payload["error_class"] != want.class {
					t.Fatalf("outcome changed: %v want %+v", payload, want)
				}
			}
			for traceID := range wants {
				if phases[traceID][catalog.KindToolCallStarted] != 1 || phases[traceID][catalog.KindToolCallFinished] != 1 {
					t.Fatalf("unmatched phases: %v", phases)
				}
			}
			projections, err := f.db.QueryProxyEvents(store.ProxyEventFilter{ToolName: "concurrent_read"})
			if err != nil || len(projections) != 3 {
				t.Fatalf("proxy row count=%d err=%v", len(projections), err)
			}
		})
	}
}
