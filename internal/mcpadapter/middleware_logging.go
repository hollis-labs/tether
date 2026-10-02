package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/telemetry"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"go.opentelemetry.io/otel/trace"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/redact"
)

// LoggingMiddleware emits tool_call_start and tool_call_end events to an
// events Publisher (the Bus, or a DaemonToolCallPublisher) for every proxied
// tool call. ArgsSchemaFP is derived from
// the sorted arg key names only — values are never logged. See ADR 0021
// §Decision 3.
type LoggingMiddleware struct {
	bus              events.Publisher
	secrets          *redact.Set
	contextDecorator func(context.Context) context.Context
	profile, mode    string
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
	if m.contextDecorator != nil {
		ctx = m.contextDecorator(ctx)
	}
	var argsRaw json.RawMessage
	if raw, err := json.Marshal(call.Args); err == nil {
		argsRaw = raw
	}
	sessionID := sessionIDFromContext(ctx)
	attribution, _ := callcontext.FromContext(ctx)
	claimed := callcontext.ClaimedSession(ctx)
	if claimed == attribution.SessionID && attribution.Verified {
		claimed = ""
	}
	if claimed == "" && !attribution.Verified {
		claimed = sessionID
	}
	server := serverIDFromContext(ctx)
	if server == "" {
		server = "tether"
	}
	if sc := trace.SpanContextFromContext(extractTraceContext(call.Meta, call.Args)); sc.IsValid() {
		ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
	}
	service := telemetry.Service{Publisher: m.bus, Secrets: m.secrets}
	ctx, observation := service.Start(ctx, telemetry.Call{Name: call.ToolName, Server: server, SessionID: sessionID, ClaimedSessionID: claimed, Fingerprint: callFingerprint(call.Args, argsRaw), ArgsBytes: int64(len(argsRaw)), Profile: m.profile, Mode: m.mode})
	result, err := next(ctx, call)
	out := telemetry.Outcome{OK: err == nil && (result == nil || !result.IsError)}
	if raw, marshalErr := json.Marshal(result); marshalErr == nil && result != nil {
		out.ResultBytes = int64(len(raw))
	}
	if err != nil {
		out.Error = err.Error()
		out.Class = telemetry.ErrorClass(err)
		var rpc *jsonrpc.Error
		if errors.As(err, &rpc) && rpc.Code == jsonrpc.CodeInvalidParams {
			out.Class = events.ToolErrorValidation
		}
		if clean := m.secrets.Redact(err.Error()); clean != err.Error() {
			err = &redactedError{text: clean, err: err}
		}
	} else if result != nil && result.IsError {
		// Copy content before scrubbing: an upstream may retain its own result.
		copyResult := *result
		copyResult.Content = append([]mcpsdk.Content(nil), result.Content...)
		for i, c := range result.Content {
			if tc, ok := c.(*mcpsdk.TextContent); ok {
				copyText := *tc
				copyText.Text = m.secrets.Redact(tc.Text)
				copyResult.Content[i] = &copyText
				if out.Error == "" {
					out.Error = copyText.Text
				}
			}
		}
		result = &copyResult
	}
	service.End(ctx, observation, out)
	return result, err
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

// Preserve the legacy empty-argument fingerprint while measuring canonical JSON.
func callFingerprint(args map[string]any, raw json.RawMessage) string {
	if len(args) == 0 {
		return argsSchemaFP(nil)
	}
	return argsSchemaFP(raw)
}
