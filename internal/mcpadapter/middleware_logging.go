package mcpadapter

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/hollis-labs/tether/internal/events"
)

// LoggingMiddleware emits tool_call_start and tool_call_end events to the
// events Bus for every proxied tool call. ArgsSchemaFP is derived from
// the sorted arg key names only — values are never logged. See ADR 0021
// §Decision 3.
type LoggingMiddleware struct {
	bus events.Bus
}

// NewLoggingMiddleware creates a LoggingMiddleware backed by bus.
// bus may be nil; when nil the middleware is a no-op pass-through.
func NewLoggingMiddleware(bus events.Bus) *LoggingMiddleware {
	return &LoggingMiddleware{bus: bus}
}

// Handle records start time, emits tool_call_start, calls next, then
// emits tool_call_end with duration and outcome.
func (m *LoggingMiddleware) Handle(ctx context.Context, req mcp.CallToolRequest, next ToolCallHandler) (*mcp.CallToolResult, error) {
	start := time.Now()

	// Compute args fingerprint — key names only, never values.
	var argsRaw json.RawMessage
	if args := req.GetArguments(); len(args) > 0 {
		if raw, err := json.Marshal(args); err == nil {
			argsRaw = raw
		}
	}
	fp := argsSchemaFP(argsRaw)

	// Extract session ID from context if present (best-effort).
	sessionID := sessionIDFromContext(ctx)

	// Look up the server ID for this tool from the registry via context, if available.
	// Native mux tools never set WithServerID, so default to "mux" to keep the
	// TUI feed readable and satisfy the POST /proxy/events tool_name-only validation.
	serverID := serverIDFromContext(ctx)
	if serverID == "" {
		serverID = "mux"
	}

	m.publish(ctx, events.EventTypeToolCallStart, events.ToolCallEvent{
		SessionID:    sessionID,
		ToolName:     req.Params.Name,
		Server:       serverID,
		ArgsSchemaFP: fp,
		Timestamp:    start,
	})

	result, err := next(ctx, req)

	durMs := time.Since(start).Milliseconds()
	ev := events.ToolCallEvent{
		SessionID:    sessionID,
		ToolName:     req.Params.Name,
		Server:       serverID,
		ArgsSchemaFP: fp,
		DurationMs:   durMs,
		OK:           err == nil && (result == nil || !result.IsError),
		Timestamp:    time.Now(),
	}
	if err != nil {
		ev.Error = err.Error()
	} else if result != nil && result.IsError {
		// Extract error text from the first text content block, if present.
		for _, c := range result.Content {
			if tc, ok := c.(mcp.TextContent); ok {
				ev.Error = tc.Text
				break
			}
		}
	}

	m.publish(ctx, events.EventTypeToolCallEnd, ev)

	slog.Debug("mcp-proxy: tool call completed",
		"tool", req.Params.Name,
		"server", serverID,
		"duration_ms", durMs,
		"ok", ev.OK,
		"error", ev.Error,
	)

	return result, err
}

// publish encodes ev as JSON and publishes it on the bus. Errors are
// logged but not returned — observability must not break the call path.
func (m *LoggingMiddleware) publish(ctx context.Context, kind string, ev events.ToolCallEvent) {
	if m.bus == nil {
		return
	}
	raw, marshalErr := json.Marshal(ev)
	if marshalErr != nil {
		slog.Warn("mcp-proxy: failed to marshal ToolCallEvent", "err", marshalErr)
		return
	}
	scope := events.ScopeSession
	if ev.SessionID == "" {
		scope = events.ScopeDaemon
	}
	if pubErr := m.bus.Publish(ctx, events.Event{
		Scope:       scope,
		SessionID:   ev.SessionID,
		Kind:        kind,
		PayloadJSON: string(raw),
	}); pubErr != nil {
		slog.Warn("mcp-proxy: failed to publish tool call event", "kind", kind, "err", pubErr)
	}
}

// ─── context key helpers ──────────────────────────────────────────────────────

type contextKey int

const (
	contextKeySessionID contextKey = iota
	contextKeyServerID
)

// WithSessionID attaches a mux session ID to ctx for the middleware chain.
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKeySessionID, id)
}

// WithServerID attaches the upstream server ID to ctx for the middleware chain.
func WithServerID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKeyServerID, id)
}

func sessionIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(contextKeySessionID).(string)
	return v
}

func serverIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(contextKeyServerID).(string)
	return v
}
