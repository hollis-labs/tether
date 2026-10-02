package mcpadapter

import (
	"context"
	"encoding/json"
	gomcp "github.com/hollis-labs/go-mcp/server"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/redact"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTelemetryPolicyRejectedCallsAreDurable(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	bus := events.NewBus(events.BusOptions{Persister: db})
	gateway := &mcpgateway.Service{Snapshot: func() mcpgateway.Snapshot {
		return mcpgateway.Snapshot{Entries: []mcpgateway.Entry{{Tool: &mcpsdk.Tool{Name: "alpha_read"}, Origin: "alpha"}}, Origins: []mcpgateway.OriginStatus{{ID: "alpha", Status: "connected"}}}
	}, Policy: &mcpgateway.Policy{Selection: mcpgateway.ProfileSelection{Profile: &mcpgateway.Profile{Tools: mcpgateway.ToolRules{Deny: []string{"alpha_*"}}}}}}
	registry := NewToolRegistry()
	if err := registry.Register("alpha", nil, []*mcpsdk.Tool{{Name: "alpha_read", InputSchema: map[string]any{"type": "object"}}}); err != nil {
		t.Fatal(err)
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "telemetry-test", Version: "1"}, nil)
	server.AddTool(&mcpsdk.Tool{Name: "alpha_read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		t.Fatal("denied call dispatched")
		return nil, nil
	})
	server.AddReceivingMiddleware(gatewaySurfaceMiddleware(gateway))
	logging := NewLoggingMiddleware(bus)
	logging.profile = "reader"
	logging.mode = "flat"
	server.AddReceivingMiddleware(proxyLoggingMiddleware([]ToolCallMiddleware{logging}, registry))
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "caller", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	result, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "alpha_read", Arguments: map[string]any{"password": "ARGUMENT-CANARY"}})
	if err != nil || !result.IsError {
		t.Fatalf("denial: %+v %v", result, err)
	}
	rows, err := db.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var end events.ToolCallEvent
	for _, row := range rows {
		if strings.Contains(row.PayloadJSON, "ARGUMENT-CANARY") {
			t.Fatal("argument value retained")
		}
		if row.Kind == events.EventTypeToolCallEnd {
			if err := json.Unmarshal([]byte(row.PayloadJSON), &end); err != nil {
				t.Fatal(err)
			}
		}
	}
	if end.ToolName != "alpha_read" || end.Server != "alpha" || end.ErrorClass != events.ToolErrorDenied || end.ArgsBytes == 0 || end.ResultBytes == 0 || end.Profile != "reader" {
		t.Fatalf("call missing: %+v", end)
	}
}
func TestTelemetryScrubsCallerErrorsWithoutMutatingUpstream(t *testing.T) {
	secrets := &redact.Set{}
	secrets.Add("CREDENTIAL-CANARY")
	logging := NewLoggingMiddleware(nil).RedactWith(secrets)
	original := errorResult("upstream echoed CREDENTIAL-CANARY")
	result, err := logging.Handle(context.Background(), ToolCall{ToolName: "read"}, func(context.Context, ToolCall) (*mcpsdk.CallToolResult, error) { return original, nil })
	if err != nil || strings.Contains(result.Content[0].(*mcpsdk.TextContent).Text, "CREDENTIAL-CANARY") {
		t.Fatal(result, err)
	}
	if !strings.Contains(original.Content[0].(*mcpsdk.TextContent).Text, "CREDENTIAL-CANARY") {
		t.Fatal("upstream result mutated")
	}
}

func TestTelemetryEmptyArgumentsHaveZeroBytes(t *testing.T) {
	for _, args := range []map[string]any{nil, {}} {
		publisher := &callCapturePublisher{}
		_, err := NewLoggingMiddleware(publisher).Handle(context.Background(), ToolCall{ToolName: "noargs", Args: args}, func(context.Context, ToolCall) (*mcpsdk.CallToolResult, error) { return &mcpsdk.CallToolResult{}, nil })
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range publisher.calls {
			if call.ArgsBytes != 0 {
				t.Fatalf("empty args size %d", call.ArgsBytes)
			}
		}
	}
}

type callCapturePublisher struct{ calls []events.ToolCallEvent }

func (p *callCapturePublisher) Publish(_ context.Context, event events.Event) error {
	var call events.ToolCallEvent
	if err := json.Unmarshal([]byte(event.PayloadJSON), &call); err != nil {
		return err
	}
	p.calls = append(p.calls, call)
	return nil
}

func TestTelemetryNativeProtocolBoundaryKeepsTypedDenialsAndScrubsStructuredError(t *testing.T) {
	native := gomcp.NewServer("native", "test")
	a := &Adapter{}
	a.addTool(native, gomcp.Tool{Name: "native_denied", InputSchema: map[string]any{"type": "object"}, Handler: func(context.Context, map[string]any) (any, error) {
		return nil, toolError("insufficient_scope", "CREDENTIAL-CANARY")
	}}, Writes())
	client := connectInMemory(t, native)
	registry := NewToolRegistry()
	if err := registry.RegisterLocal(&mcpsdk.Tool{Name: "native_denied"}, client); err != nil {
		t.Fatal(err)
	}
	router := NewProxyRouter(registry)
	publisher := &callCapturePublisher{}
	secrets := &redact.Set{}
	secrets.Add("CREDENTIAL-CANARY")
	result, err := NewLoggingMiddleware(publisher).RedactWith(secrets).Handle(context.Background(), ToolCall{ToolName: "native_denied"}, router.Handle)
	if err != nil || !result.IsError {
		t.Fatal(result, err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "CREDENTIAL-CANARY") {
		t.Fatal("credential in structured caller error")
	}
	for _, call := range publisher.calls {
		if call.ErrorClass != "" && call.ErrorClass != events.ToolErrorDenied {
			t.Fatalf("native denial misclassified: %+v", call)
		}
	}
	if len(publisher.calls) != 2 || publisher.calls[1].ErrorClass != events.ToolErrorDenied {
		t.Fatal(publisher.calls)
	}
}
