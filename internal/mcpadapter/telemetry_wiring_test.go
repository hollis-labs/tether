package mcpadapter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type wiringEventSink chan events.Event

func (sink wiringEventSink) Publish(_ context.Context, event events.Event) error {
	sink <- event
	return nil
}

func TestTelemetryDaemonRecorderPreservesResolvedContextAndMetadata(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	sink := make(wiringEventSink, 1)
	recorder := NewDaemonToolCallRecorder(ctx, sink, db)
	defer func() { cancel(); <-recorder.done }()
	call := events.ToolCallEvent{ToolName: "read", Attribution: callcontext.Snapshot{Verified: true, PrincipalID: "resolved-principal"}, ToolCallDetails: events.ToolCallDetails{Profile: "reader", DiscoveryMode: "search", ArgsBytes: 12, ResultBytes: 24, ErrorClass: events.ToolErrorDenied, ErrorTruncated: true, GatewayMs: 3, ForwardMs: 4, TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef"}}
	raw, _ := json.Marshal(call)
	if err := recorder.Publish(ctx, events.Event{Kind: events.EventTypeToolCallEnd, PayloadJSON: string(raw)}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sink:
		var persisted events.ToolCallEvent
		if err := json.Unmarshal([]byte(event.PayloadJSON), &persisted); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryProxyEvents(store.ProxyEventFilter{})
		if err != nil || len(rows) != 1 {
			t.Fatalf("projection: %+v %v", rows, err)
		}
		if rows[0].ToolCallDetails != call.ToolCallDetails || rows[0].Attribution != call.Attribution || persisted.ToolCallDetails != call.ToolCallDetails || persisted.Attribution != call.Attribution {
			t.Fatalf("resolved call changed: %+v / %+v", rows[0], persisted)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon recorder did not persist call")
	}
}

func TestTelemetrySharedViewPolicyDenialsCarryProfileAndMode(t *testing.T) {
	r, err := NewSharedUpstreams(nil, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, mode := range []string{"flat", "search"} {
		t.Run(mode, func(t *testing.T) {
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "reader", Kind: "service"})
			a, err := NewVerifiedAdapter(ctx, &app.Service{Catalog: &config.Catalog{}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			sink := make(wiringEventSink, 2)
			v, err := r.NewGatewayView(ctx, a, ProxyOptions{Only: true, ServerFilter: []string{}, Publisher: sink, Profile: mcpgateway.ProfileSelection{ID: "reader", Profile: &mcpgateway.Profile{}}, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: mode, Source: "test"}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			client := connectDaemonView(ctx, t, v)
			name, args := "excluded_tool", map[string]any{}
			if mode == "search" {
				name, args = "tether_tool_call", map[string]any{"name": "excluded_tool", "arguments": args}
			}
			result, err := client.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
			if err != nil || !result.IsError {
				t.Fatalf("denial: %+v %v", result, err)
			}
			<-sink // start
			var call events.ToolCallEvent
			if err := json.Unmarshal([]byte((<-sink).PayloadJSON), &call); err != nil {
				t.Fatal(err)
			}
			if call.ErrorClass != events.ToolErrorDenied || call.Profile != "reader" || call.DiscoveryMode != mode || call.ToolName != "excluded_tool" || call.ForwardMs != 0 {
				t.Fatalf("denied observation: %+v", call)
			}
		})
	}
}

func TestTelemetryRouterRecordsForwardLatencyAndTypedFailures(t *testing.T) {
	for _, scenario := range []string{"ok", "timeout", "validation", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			registry := NewToolRegistry()
			var client upstreamClient
			if scenario != "unavailable" {
				client = &mockClient{callToolFunc: func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
					time.Sleep(3 * time.Millisecond)
					if scenario == "timeout" {
						return nil, context.DeadlineExceeded
					}
					if scenario == "validation" {
						return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid input"}
					}
					return &mcpsdk.CallToolResult{}, nil
				}}
			}
			mustRegister(t, registry, "alpha", client, []*mcpsdk.Tool{makeTool("read")})
			capture := &callCapturePublisher{}
			_, _ = NewLoggingMiddleware(capture).Handle(WithServerID(context.Background(), "alpha"), ToolCall{ToolName: "read"}, NewProxyRouter(registry).Handle)
			call := capture.calls[1]
			if scenario == "unavailable" {
				if call.ForwardMs != 0 || call.ErrorClass != events.ToolErrorUpstreamDown {
					t.Fatal(call)
				}
			} else if call.ForwardMs < 1 || call.DurationMs < call.ForwardMs {
				t.Fatal(call)
			} else if scenario == "timeout" && call.ErrorClass != events.ToolErrorTimeout {
				t.Fatal(call)
			} else if scenario == "validation" && call.ErrorClass != events.ToolErrorValidation {
				t.Fatal(call)
			}
		})
	}
}
