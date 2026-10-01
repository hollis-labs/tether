package mcpadapter

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/redact"
)

// LoggingMiddleware emits tool_call_start and tool_call_end events to an
// events Publisher (the Bus, or a DaemonToolCallPublisher) for every proxied
// tool call. ArgsSchemaFP is derived from
// the sorted arg key names only — values are never logged. See ADR 0021
// §Decision 3.
type LoggingMiddleware struct {
	bus     events.Publisher
	secrets *redact.Set
}

// NewLoggingMiddleware creates a LoggingMiddleware backed by bus.
// bus may be nil; when nil the middleware is a no-op pass-through.
func NewLoggingMiddleware(bus events.Publisher) *LoggingMiddleware {
	return &LoggingMiddleware{bus: bus}
}

// RedactWith makes the middleware scrub secrets from a call's error text
// before the event is published, and so before it reaches
// proxy_events.error (CW-20260930-0009). An upstream server's validation
// error can echo an argument value back verbatim, and argument values are
// otherwise never recorded (ADR 0021 Decision 3).
func (m *LoggingMiddleware) RedactWith(secrets *redact.Set) *LoggingMiddleware {
	m.secrets = secrets
	return m
}

// proxyRedactionSet collects the values scrubbed from tool-call error text:
// every configured server's token, resolved argument secrets and env values,
// the same set upstream stderr is scrubbed with. It spans all servers, not
// only the one called, because an agent can pass one server's credential to
// another server's tool, which may echo it back in an error.
func proxyRedactionSet(entries []config.MCPServerEntry) *redact.Set {
	secrets := &redact.Set{}
	for _, e := range entries {
		secrets.Add(stderrRedactionValues(e)...)
	}
	return secrets
}

// Handle records start time, emits tool_call_start, calls next, then
// emits tool_call_end with duration and outcome.
func (m *LoggingMiddleware) Handle(ctx context.Context, call ToolCall, next ToolCallHandler) (*mcpsdk.CallToolResult, error) {
	start := time.Now()

	// Compute args fingerprint — key names only, never values.
	var argsRaw json.RawMessage
	if len(call.Args) > 0 {
		if raw, err := json.Marshal(call.Args); err == nil {
			argsRaw = raw
		}
	}
	fp := argsSchemaFP(argsRaw)

	// Extract session ID from context if present (best-effort).
	sessionID := sessionIDFromContext(ctx)

	// Look up the server ID for this tool from the registry via context, if available.
	// Native tether tools never set WithServerID, so default to "tether" to keep the
	// TUI feed readable and satisfy the POST /proxy/events tool_name-only validation.
	serverID := serverIDFromContext(ctx)
	if serverID == "" {
		serverID = "tether"
	}

	m.publish(ctx, events.EventTypeToolCallStart, events.ToolCallEvent{
		SessionID:    sessionID,
		ToolName:     call.ToolName,
		Server:       serverID,
		ArgsSchemaFP: fp,
		Timestamp:    start,
	})

	result, err := next(ctx, call)

	durMs := time.Since(start).Milliseconds()
	ev := events.ToolCallEvent{
		SessionID:    sessionID,
		ToolName:     call.ToolName,
		Server:       serverID,
		ArgsSchemaFP: fp,
		DurationMs:   durMs,
		OK:           err == nil && (result == nil || !result.IsError),
		Timestamp:    time.Now(),
	}
	if err != nil {
		ev.Error = m.secrets.Redact(err.Error())
	} else if result != nil && result.IsError {
		// Extract error text from the first text content block, if present.
		for _, c := range result.Content {
			if tc, ok := c.(*mcpsdk.TextContent); ok {
				ev.Error = m.secrets.Redact(tc.Text)
				break
			}
		}
	}

	m.publish(ctx, events.EventTypeToolCallEnd, ev)

	slog.Debug("mcp-proxy: tool call completed",
		"tool", call.ToolName,
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

// WithSessionID attaches a tether session ID to ctx for the middleware chain.
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
