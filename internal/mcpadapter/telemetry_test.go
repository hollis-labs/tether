package mcpadapter

import (
	"context"
	"encoding/json"
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
