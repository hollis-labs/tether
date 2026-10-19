package mcpadapter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/eventstamp"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestCanonicalToolEventsThroughDaemonRecorder(t *testing.T) {
	previous := otel.GetTracerProvider()
	provider := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(provider)
	defer func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) }()
	for _, scenario := range []string{"success", "result_error", "transport_timeout", "unmanaged", "anonymous"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDaemonOnlyFixture(t)
			bus := events.NewBus(events.BusOptions{Persister: f.db, Stages: []events.Stage{eventstamp.New()}})
			root, cancel := context.WithCancel(context.Background())
			recorder := NewDaemonToolCallRecorder(root, bus, f.db)
			defer func() { cancel(); <-recorder.done }()
			ctx := root
			bound := scenario != "unmanaged" && scenario != "anonymous"
			snap := callcontext.Snapshot{Verified: true, SessionResolved: true, SessionID: "sess-1", AgentURN: "msg://agent/local/worker", TaskID: "task", WorkstreamID: "work", ProjectID: "project", LaunchID: "launch"}
			if bound {
				ctx = identity.WithPrincipal(ctx, identity.Principal{Verified: true, Kind: "session", SessionID: "sess-1"})
				ctx = callcontext.WithSnapshot(ctx, snap)
			} else if scenario == "unmanaged" {
				ctx = identity.WithPrincipal(ctx, identity.Principal{Verified: true, ID: "msg://user/local/operator", Kind: "operator"})
				ctx = callcontext.WithSnapshot(ctx, callcontext.Snapshot{Source: "daemon", Verified: true, PrincipalID: "msg://user/local/operator", SessionID: "claimed", TaskID: "forged", ProjectID: "forged"})
			}
			ctx = WithSessionID(ctx, "claimed")
			if bound {
				ctx = WithSessionID(ctx, "sess-1")
			}
			_, _ = NewLoggingMiddleware(recorder).Handle(ctx, ToolCall{ToolName: "read", Args: map[string]any{"secret": "ARGUMENT-CANARY"}}, func(context.Context, ToolCall) (*mcpsdk.CallToolResult, error) {
				if scenario == "transport_timeout" {
					return nil, context.DeadlineExceeded
				}
				if scenario == "result_error" {
					return errorResult("RESULT-CANARY"), nil
				}
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "RESULT-CANARY"}}}, nil
			})
			var rows []events.Event
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				var err error
				all, readErr := f.db.EventsSince(0)
				err = readErr
				rows = nil
				for _, row := range all {
					switch row.Kind {
					case events.EventTypeToolCallStart, events.EventTypeToolCallEnd, catalog.KindToolCallStarted, catalog.KindToolCallFinished:
						rows = append(rows, row)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) == 4 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if len(rows) != 4 {
				t.Fatalf("want legacy+canonical pair, got %d", len(rows))
			}
			wantKinds := []string{events.EventTypeToolCallStart, catalog.KindToolCallStarted, events.EventTypeToolCallEnd, catalog.KindToolCallFinished}
			for i, row := range rows {
				if row.Kind != wantKinds[i] {
					t.Fatalf("kind[%d]=%s", i, row.Kind)
				}
			}
			for _, row := range []events.Event{rows[1], rows[3]} {
				var payload map[string]any
				if err := json.Unmarshal([]byte(row.PayloadJSON), &payload); err != nil {
					t.Fatal(err)
				}
				for key := range payload {
					switch key {
					case "tool", "server", "args_bytes", "args_schema_fp", "ok", "error_class", "duration_ms", "gateway_ms", "forward_ms", "result_bytes":
					default:
						t.Fatalf("unexpected metadata field %s", key)
					}
				}
				if payload["tool"] != "read" || payload["args_bytes"].(float64) <= 0 {
					t.Fatal(payload)
				}
				if row.Envelope.Corr.TraceID == "" || row.Envelope.Corr.SpanID == "" || row.Envelope.Corr != rows[1].Envelope.Corr {
					t.Fatalf("unmatched correlation: %+v", row.Envelope.Corr)
				}
				if bound {
					if row.SessionID != "sess-1" || row.Envelope.Dims.TaskID != "task" || row.Envelope.Dims.WorkstreamID != "work" || row.Envelope.Dims.ProjectID != "project" || row.Envelope.Actor.URN != snap.AgentURN || row.Envelope.Actor.Verified != "token" {
						t.Fatalf("lost verified dimensions: %+v", row.Envelope)
					}
				} else {
					if row.SessionID != "" || row.Scope != events.ScopeDaemon || row.Envelope.Dims != (events.ScopeDims{}) {
						t.Fatalf("claim became dimensions: %+v", row)
					}
					if scenario == "unmanaged" && row.Envelope.Actor.URN != "msg://user/local/operator" {
						t.Fatal("operator actor lost")
					}
					if scenario == "anonymous" && row.Envelope.Actor.URN != "" {
						t.Fatal("anonymous actor invented")
					}
				}
				if row.Kind == catalog.KindToolCallStarted {
					if _, ok := payload["ok"]; ok {
						t.Fatal("started carries outcome")
					}
					if len(payload) != 4 {
						t.Fatal("started carries finish metadata")
					}
				} else {
					wantOK := scenario != "result_error" && scenario != "transport_timeout"
					if payload["ok"] != wantOK || payload["result_bytes"].(float64) < 0 {
						t.Fatal(payload)
					}
					if scenario == "transport_timeout" && payload["error_class"] != "timeout" {
						t.Fatal(payload)
					}
					if scenario == "result_error" && payload["error_class"] != "upstream_error" {
						t.Fatal(payload)
					}
				}
			}
			projections, err := f.db.QueryProxyEvents(store.ProxyEventFilter{ToolName: "read"})
			if err != nil || len(projections) != 1 {
				t.Fatalf("legacy projection count=%d err=%v", len(projections), err)
			}
		})
	}
}
