// Package mcpadapter exposes the agent-mux runtime as an MCP stdio server.
//
// Start the server with:
//
//	mux mcp [--token <tok>] [--scopes session.write,message.write]
//
// The adapter wraps app.Service for catalog reads and read-only session
// inspection. Session-mutating tools (create, launch, stop, send, resize,
// wait, logical-agent resume) AND all message tools (send/notify/get/
// inbox/list/thread/consume/cancel/mark_read/archive/unarchive) route
// through the running muxd daemon over UDS via internal/client.Client to
// avoid the split-brain that an in-process app.New() instance would
// otherwise produce against daemon-owned session/message state — this
// process (`mux mcp`) always opens its own separate SQLite connection to
// the same database file for catalog/session-read purposes, so any tool
// that WRITES messaging state must not touch that connection directly
// (T05, messaging vNext: closed the pre-T05 gap where message tools called
// the in-process store, bypassing the daemon's authorization/fan-out
// entirely — see planning/docs/messaging-vnext/T01-compatibility-contract.md
// §2.7). v005-09 introduced the daemon routing for session tools — see
// ADR 0034 (or 0035 if split). All mutating tools require a token and the
// appropriate scope string.
//
// Scopes:
//
//	session.write  — create, launch, stop, wait, input, resize, resume
//	message.write  — send, consume, cancel
//	catalog.write  — agent create/edit (catalog file writes)
package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	mcpsanitize "github.com/hollis-labs/go-mcp-sanitize"
	hotel "github.com/hollis-labs/go-otel"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel/trace"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
)

const version = "0.2.0"

// Scope constants for mutating tool groups.
const (
	ScopeSessionWrite = "session.write"
	ScopeMessageWrite = "message.write"
	// ScopeAIInvoke gates model-invocation tools on the AI gateway surface.
	// Read-side AI introspection and durable audit/usage queries remain
	// scope-free.
	ScopeAIInvoke = "ai.invoke"
	// ScopeCatalogWrite gates tools that write catalog files (agent
	// create/edit). It is deliberately separate from session/message
	// scopes so an operator can grant runtime control without granting
	// the ability to mutate catalog definitions, and vice versa.
	ScopeCatalogWrite = "catalog.write"
)

// Adapter exposes the agent-mux runtime as MCP tools over stdio.
type Adapter struct {
	svc    *app.Service
	client *client.Client // optional; when set, session-mutating tools route through the daemon
	mcp    *server.MCPServer
	token  string
	scopes map[string]struct{}

	// Logger receives the warn-level telemetry emitted by the
	// go-mcp-sanitize middleware when it cleans a polluted tool call.
	// Optional; nil falls back to slog.Default(). The MCP stdio command
	// wires this to stderr so warn lines do not collide with the protocol
	// stream on stdout.
	Logger *slog.Logger

	// SessionID is the Tether session this adapter process serves, from
	// `mux mcp --session`. Empty when the proxy is reached by something with
	// no Tether session — a hand-launched client, or boot-exec — which is a
	// legitimate state, not a misconfiguration.
	//
	// It is attached to every tool-call context via WithSessionID, which the
	// logging middleware already reads for proxy_events and which S3's
	// extraction needs to attribute a ref. Before CW-20260912-0074 nothing
	// ever called WithSessionID, so every proxy_events row recorded an
	// unknown session and nobody was told.
	SessionID string

	// ExtractRefs enables S3's proxy-side identifier extraction. Off by
	// default: this is the first thing in the proxy to capture argument
	// VALUES rather than shapes, so it is opt-in until it has been watched on
	// real traffic. See extract.go for what it does and does not store.
	ExtractRefs bool

	// refs is where extracted refs are written. Nil disables extraction
	// regardless of ExtractRefs -- the proxy runs in its own process and
	// reaches the daemon over HTTP, so with no client there is nowhere to
	// write.
	refs refAttacher
}

// New constructs an Adapter wrapping svc. token and scopes gate mutating
// tools; pass an empty token to disable auth (development only).
//
// In this mode session-mutating tools execute against svc directly
// in-process. Use NewWithDaemon for the production "mux mcp" path so
// session ownership stays with the daemon.
func New(svc *app.Service, token string, scopes []string) *Adapter {
	scopeSet := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s != "" {
			scopeSet[s] = struct{}{}
		}
	}
	return &Adapter{
		svc:    svc,
		token:  strings.TrimSpace(token),
		scopes: scopeSet,
	}
}

// NewWithDaemon constructs an Adapter that routes session-mutating tools
// AND all message tools through the running muxd daemon at dc, while
// keeping catalog reads and read-only session inspection in-process via
// svc.
//
// Use this for the production "mux mcp" subcommand. dc must not be nil
// — pass New for in-process-only mode (message tools then return a clear
// "requires daemon routing" error rather than silently touching a second,
// unfan-out'd SQLite connection).
func NewWithDaemon(svc *app.Service, dc *client.Client, token string, scopes []string) *Adapter {
	a := New(svc, token, scopes)
	a.client = dc
	return a
}

// Run starts the MCP stdio server. It blocks until ctx is canceled or
// the stdio transport closes.
func (a *Adapter) Run(ctx context.Context) error {
	s := server.NewMCPServer(
		"agent-mux",
		version,
		server.WithToolCapabilities(true),
	)
	a.registerTools(s)
	ctxFunc := func(_ context.Context) context.Context { return ctx }
	return server.ServeStdio(s, server.WithStdioContextFunc(ctxFunc))
}

// addTool wraps every MCP tool handler with the go-mcp-sanitize middleware,
// which auto-cleans malformed agent tool-call XML in free-text params before
// the handler runs. Clean calls are silent; cleaned calls emit one warn-level
// slog line via a.Logger (see github.com/hollis-labs/go-mcp-sanitize).
//
// All registerXxx helpers must call a.addTool(s, tool, handler) instead of
// s.AddTool(tool, handler) directly so the protection stays uniform across
// every tool surface registered by the adapter.
//
// b is REQUIRED and has no usable zero value: a tool cannot be registered
// without stating what it does. See behavior.go for why, and for the precise
// statement of what that does and does not prevent (it prevents omission, not
// a wrong value).
//
// An unset Behavior panics rather than registering. That is deliberate: every
// tool registers at process start, so the failure is immediate, total and
// deterministic in every run and every test -- it cannot ship. Publishing a
// zero-valued Behavior would advertise readOnly=false, destructive=false, the
// most permissive tuple of all, which is the opposite of the cautious default
// this work is replacing.
func (a *Adapter) addTool(s *server.MCPServer, t mcp.Tool, b Behavior, h server.ToolHandlerFunc) {
	if !b.valid() {
		panic("mcpadapter: tool " + t.Name + " registered with an unset Behavior; use Reads, Writes or Destroys")
	}
	b.annotations()(&t)
	logger := a.Logger
	if logger == nil {
		logger = slog.Default()
	}
	handler := mcpsanitize.Middleware(logger)(h)
	s.AddTool(t, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx = a.withSessionID(ctx)
		if sc := trace.SpanContextFromContext(extractTraceContext(req)); sc.IsValid() {
			ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
		}
		ctx, span := hotel.ToolCallSpan(ctx, t.Name)
		defer span.End()
		return handler(ctx, req)
	})
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// toolJSON serializes v as a JSON text tool result.
func toolJSON(v any) *mcp.CallToolResult {
	b, _ := json.Marshal(v)
	return mcp.NewToolResultText(string(b))
}

// toolError returns an MCP error result (IsError=true) with a structured JSON body.
// Using NewToolResultError ensures middleware and clients that check result.IsError
// correctly identify failures — NewToolResultText with ok:"false" was wrong.
func toolError(code, message string) *mcp.CallToolResult {
	b, _ := json.Marshal(map[string]any{"ok": false, "code": code, "message": message})
	return mcp.NewToolResultError(string(b))
}

// daemonUnreachableError is the canonical tool-error response when a
// session-mutating tool routes through the daemon but the daemon is not
// running or otherwise unreachable. Code "daemon_unavailable" matches
// ADR 0010's typed error envelope conventions; the message is actionable.
func daemonUnreachableError(err error) *mcp.CallToolResult {
	return toolError("daemon_unavailable",
		"muxd daemon is not reachable; start it with `mux daemon up` ("+err.Error()+")")
}

// isDaemonUnreachable reports whether err signals that the daemon is not
// running (socket missing, connection refused, dial timeout). Wraps
// client.ErrDaemonUnreachable so handlers can branch on transport-vs-domain
// failure consistently.
func isDaemonUnreachable(err error) bool {
	return errors.Is(err, client.ErrDaemonUnreachable)
}

// classifyClientErr maps a daemon HTTP error string into the same MCP
// error codes the in-process path produces (invalid_request / not_found /
// conflict / internal_error). The daemon already classifies via ADR 0010 typed
// envelopes; we string-sniff the wrapped form ("daemon NNN (code): msg")
// to recover the code without reaching into internal/api here.
func classifyClientErr(err error, id string) *mcp.CallToolResult {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "(invalid_request)"), strings.Contains(msg, " 400 "):
		return toolError("invalid_request", err.Error())
	case strings.Contains(msg, "(not_found)"), strings.Contains(msg, " 404 "):
		if id == "" {
			return toolError("not_found", err.Error())
		}
		return toolError("not_found", "session not found: "+id)
	case strings.Contains(msg, "(conflict)"), strings.Contains(msg, " 409 "):
		return toolError("conflict", err.Error())
	default:
		return toolError("internal_error", err.Error())
	}
}

// checkScope verifies that the token is set and the named scope is present.
// Returns a non-nil error result when the check fails.
func (a *Adapter) checkScope(scope string) *mcp.CallToolResult {
	if a.token == "" {
		return toolError("auth_required", "no token configured; pass --token to enable mutating tools")
	}
	if _, ok := a.scopes[scope]; !ok {
		return toolError("insufficient_scope", "token missing required scope: "+scope)
	}
	return nil
}

// str extracts a string argument from a CallToolRequest, returning "" if
// the key is absent or not a string.
func str(req mcp.CallToolRequest, key string) string {
	v, _ := req.GetArguments()[key].(string)
	return strings.TrimSpace(v)
}

// intArg extracts an integer argument. JSON numbers arrive as float64 from
// well-behaved callers, but LLM clients frequently emit numeric strings (e.g.
// "50"). Both forms are handled; unknown types fall back to def.
func intArg(req mcp.CallToolRequest, key string, def int) int {
	switch v := req.GetArguments()[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
		return def
	case string:
		if v == "" {
			return def
		}
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
		return def
	default:
		return def
	}
}

// floatArg extracts a floating-point argument. Accepts JSON numbers,
// json.Number, and numeric strings; anything else falls back to def.
func floatArg(req mcp.CallToolRequest, key string, def float64) float64 {
	switch v := req.GetArguments()[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case json.Number:
		if n, err := v.Float64(); err == nil {
			return n
		}
		return def
	case string:
		if v == "" {
			return def
		}
		var n float64
		if _, err := fmt.Sscanf(v, "%f", &n); err == nil {
			return n
		}
		return def
	default:
		return def
	}
}

func strSliceArg(req mcp.CallToolRequest, key string) []string {
	raw, ok := req.GetArguments()[key]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			item = strings.TrimSpace(item)
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				continue
			}
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// withSessionID attaches this adapter's session to ctx, so the logging
// middleware and the proxy extraction path can attribute the call. A no-op
// when the adapter has no session, which leaves sessionIDFromContext
// answering "" exactly as it did before -- an absent attribution rather than
// a wrong one.
func (a *Adapter) withSessionID(ctx context.Context) context.Context {
	if a.SessionID == "" {
		return ctx
	}
	return WithSessionID(ctx, a.SessionID)
}

// SetRefAttacher wires where extracted session refs are written. Separate from
// the constructors because extraction is opt-in and only the `mux mcp` command
// has the daemon client to supply.
func (a *Adapter) SetRefAttacher(r refAttacher) { a.refs = r }

// logger returns the adapter's logger or slog's default, matching addTool.
func (a *Adapter) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}
