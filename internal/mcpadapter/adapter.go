// Package mcpadapter exposes the tether runtime as an MCP stdio server.
//
// Start the server with:
//
//	tether mcp [--token <tok>] [--scopes session.write,message.write]
//
// The adapter wraps app.Service for catalog reads and read-only session
// inspection. Session-mutating tools (create, launch, stop, send, resize,
// wait, logical-agent resume) AND all message tools (send/notify/get/
// inbox/list/thread/consume/cancel/mark_read/archive/unarchive) route
// through the running tetherd daemon over UDS via internal/client.Client to
// avoid the split-brain that an in-process app.New() instance would
// otherwise produce against daemon-owned session/message state — this
// process (`tether mcp`) always opens its own separate SQLite connection to
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

	"github.com/hollis-labs/libs/plugin-mcp/go-mcp/budget"
	"github.com/hollis-labs/libs/plugin-mcp/go-mcp/sanitize"
	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"
	hotel "github.com/hollis-labs/libs/util/otel"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/telemetry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"
)

// Scope constants for mutating tool groups.
const (
	ScopeSessionWrite = "session.write"
	ScopeMessageWrite = "message.write"
	// ScopeTeamWrite gates all team verb tools; service grants remain authoritative.
	ScopeTeamWrite = "team.write"
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

// Adapter exposes the tether runtime as MCP tools over stdio.
type Adapter struct {
	teams                 api.TeamOps
	runtime               RuntimeObservation                          // captured from this process, never the installed path
	upstreams             interface{ StatusSummary() []ServerStatus } // scoped source for daemon views; legacy pool otherwise
	svc                   *app.Service
	client                *client.Client // optional; when set, session-mutating tools route through the daemon
	mcp                   *gomcp.Server
	token                 string
	scopes                map[string]struct{}
	principal             *identity.Principal // daemon view only; derived from verified middleware
	connectionContext     context.Context     // accepted-socket proof for in-memory native dispatch
	callerContextResolver func(context.Context) (callcontext.Snapshot, error)
	callerContextCache    callerContextCache

	// protected is the set of directories this adapter must not write, as real
	// paths (SetProtectedPaths). Tether sets it for the `tether mcp` it plants into
	// a launched agent, from the same decision that registers the agent's
	// ProtectedPaths (CW-20261001-0142). It is a POLICY, not a side effect of a
	// read-only mount: a runtime that runs the planted server outside Tether's
	// sandbox (Codex spawns MCP servers itself, unsandboxed) gets the same
	// refusal as one that runs it inside.
	protected []string

	// Logger receives the warn-level telemetry emitted by the
	// go-mcp-sanitize middleware when it cleans a polluted tool call.
	// Optional; nil falls back to slog.Default(). The MCP stdio command
	// wires this to stderr so warn lines do not collide with the protocol
	// stream on stdout.
	Logger *slog.Logger

	// SessionID is the Tether session this adapter process serves, from
	// `tether mcp --session`. Empty when the proxy is reached by something with
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

	// resolver is the seam for resolving key-only captures to typed refs via
	// tesseract_ref_resolve (CW-20260914-0003).
	resolver RefResolver
}

// New constructs an Adapter wrapping svc. token and scopes gate mutating
// tools; pass an empty token to disable auth (development only).
//
// In this mode session-mutating tools execute against svc directly
// in-process. Use NewWithDaemon for the production "tether mcp" path so
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
		runtime: processObservation,
		svc:     svc,
		token:   strings.TrimSpace(token),
		scopes:  scopeSet,
	}
}

// NewWithDaemon constructs an Adapter that routes session-mutating tools
// AND all message tools through the running tetherd daemon at dc, while
// keeping catalog reads and read-only session inspection in-process via
// svc.
//
// Use this for the production "tether mcp" subcommand. dc must not be nil
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
	return a.RunWithGatewayOpts(ctx, "", ProxyOptions{}, false)
}

// newBareServer builds a go-mcp server advertising the RuntimeObservation
// experimental capability, with the sanitize middleware installed once at
// the transport-dispatch layer (rather than per-tool, as mark3labs
// required) -- and nothing else registered. Shared by newServer (the plain
// stdio path) and RunWithProxyOpts (proxy_adapter.go), which each register a
// different tool set on top of it.
func (a *Adapter) newBareServer(options ...gomcp.Option) *gomcp.Server {
	if a.principal == nil {
		options = append(options, gomcp.WithCapabilities(&mcpsdk.ServerCapabilities{Experimental: map[string]any{RuntimeObservationCapability: a.runtime}}))
	}
	s := gomcp.NewServer("tether", a.runtime.Build.Version, options...)
	s.SDKServer().AddReceivingMiddleware(sanitize.Middleware(a.logger()))
	return s
}

// newServer builds a fully-configured go-mcp server with every native tool
// registered.
func (a *Adapter) newServer() *gomcp.Server {
	s := a.newBareServer()
	a.registerTools(s)
	a.registerDocsResources(s)
	return s
}

// addTool registers t on s after stamping its required annotation hints from
// b and wrapping its handler with session/trace-context attachment and a
// tool-call span. Every registerXxx helper must go through addTool instead of
// s.RegisterTool directly so that coverage stays uniform across every tool
// surface the adapter exposes -- sanitize protection itself is now installed
// once, globally, in newServer (see go-mcp's sanitize.Middleware, which runs
// as a receiving middleware over every tools/call request rather than a
// per-tool wrapper).
//
// b is REQUIRED and has no usable zero value: a tool cannot be registered
// without stating what it does. See behavior.go for why, and for the precise
// statement of what that does and does not prevent (it prevents omission, not
// a wrong value).
//
// An unset Behavior panics rather than registering. That is deliberate: every
// tool registers at process start, so the failure is immediate, total and
// deterministic in every run and every test -- it cannot ship.
func (a *Adapter) addTool(s *gomcp.Server, t gomcp.Tool, b Behavior) {
	if !b.valid() {
		panic("mcpadapter: tool " + t.Name + " registered with an unset Behavior; use Reads, Writes or Destroys")
	}
	ann := b.annotations()
	name := t.Name
	inner := t.Handler
	t.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		ctx = a.withClaimedSessionID(ctx)
		if sc := trace.SpanContextFromContext(extractTraceContext(ctx, gomcp.MetaFromContext(ctx), args)); sc.IsValid() && !telemetry.IsObserved(ctx) {
			ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
		}
		ctx, span := hotel.ToolCallSpan(ctx, name)
		defer span.End()
		out, err := inner(ctx, args)
		var failure *budget.ToolError
		if errors.As(err, &failure) {
			switch failure.Code {
			case "auth_required", "insufficient_scope", "unauthenticated", "denied":
				telemetry.SetErrorClass(ctx, events.ToolErrorDenied)
			case "invalid_request", "bad_request":
				telemetry.SetErrorClass(ctx, events.ToolErrorValidation)
			case "daemon_unavailable", "unavailable":
				telemetry.SetErrorClass(ctx, events.ToolErrorUpstreamDown)
			}
		}
		return out, err
	}
	t.ReadOnlyHint = ann.readOnly
	t.DestructiveHint = ann.destructive
	t.IdempotentHint = ann.idempotent
	t.OpenWorldHint = ann.openWorld
	s.RegisterTool(t)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// toolJSON marks v as a tool's JSON result. Under go-mcp's ToolHandler
// contract a non-string return value already becomes the JSON result
// (StructuredContent, mirrored as text) with no wrapping required; this stays
// as a documenting identity so call sites still read "this is the JSON
// result" rather than an unmarked map/struct literal.
func toolJSON(v any) any {
	return v
}

// toolError returns a *budget.ToolError, which go-mcp reports as error
// content with IsError=true and the full structured shape (code, field,
// retryable, next step, help tool) preserved in StructuredContent -- the
// same correctness toolError's callers relied on NewToolResultError for
// before this package carried its own error type.
func toolError(code, message string) *budget.ToolError {
	return budget.NewToolError(code, message)
}

// daemonUnreachableError is the canonical tool-error response when a
// session-mutating tool routes through the daemon but the daemon is not
// running or otherwise unreachable. Code "daemon_unavailable" matches
// ADR 0010's typed error envelope conventions; the message is actionable.
func daemonUnreachableError(err error) *budget.ToolError {
	return toolError("daemon_unavailable",
		"tetherd daemon is not reachable; start it with `tether daemon up` ("+err.Error()+")")
}

// readsViaDaemon reports whether the adapter reads the state database over
// the daemon API rather than in-process. A daemon-only adapter (`tether mcp
// --daemon-only`, the server Tether plants in each agent) has a client and no
// Store: it never opens the database, so an agent's sandbox can keep the
// state directory read-only (CW-20261001-0173).
func (a *Adapter) readsViaDaemon() bool {
	return a.client != nil && (a.svc == nil || a.svc.Store == nil)
}

// daemonReadError maps a failed daemon read to a tool error: the daemon being
// down, or the classified HTTP error. id names the session for a not_found.
func daemonReadError(err error, id string) *budget.ToolError {
	if isDaemonUnreachable(err) {
		return daemonUnreachableError(err)
	}
	return classifyClientErr(err, id)
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
func classifyClientErr(err error, id string) *budget.ToolError {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "(invalid_request)"), strings.Contains(msg, " 400 "):
		return toolError("invalid_request", err.Error())
	case strings.Contains(msg, "(not_found)"), strings.Contains(msg, " 404 "):
		if id == "" {
			return toolError("not_found", err.Error())
		}
		return toolError("not_found", "session not found: "+id)
	case strings.Contains(msg, "(idempotency_conflict)"):
		return toolError("idempotency_conflict", err.Error())
	case strings.Contains(msg, "(provider_session_lost)"):
		return toolError("provider_session_lost", err.Error())
	case strings.Contains(msg, "(turn_failed)"):
		return toolError("turn_failed", err.Error())
	case strings.Contains(msg, "(conflict)"), strings.Contains(msg, " 409 "):
		return toolError("conflict", err.Error())
	default:
		return toolError("internal_error", err.Error())
	}
}

// checkScope verifies that the token is set and the named scope is present.
// Returns a non-nil error when the check fails.
func (a *Adapter) checkScope(scope string) error {
	if a.token == "" {
		return toolError("auth_required", "no token configured; pass --token to enable mutating tools")
	}
	if a.principal != nil {
		if _, all := a.scopes["*"]; all {
			return nil
		}
	}
	if _, ok := a.scopes[scope]; !ok {
		return toolError("insufficient_scope", "token missing required scope: "+scope)
	}
	return nil
}

// checkToken verifies only that a token is configured, returning the same
// auth_required error checkScope does. It gates the AI usage, budget and
// audit reads (CW-20260930-0011): those must not be open to a tokenless
// caller, but have no scope of their own, and no client configured today
// holds ai.invoke, so requiring it would lock them all out.
func (a *Adapter) checkToken() error {
	if a.token == "" {
		return toolError("auth_required", "no token configured; pass --token to read AI usage, budget and audit data")
	}
	return nil
}

// str extracts a string argument from a tool call's decoded arguments,
// returning "" if the key is absent or not a string.
func str(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

// intArg extracts an integer argument. JSON numbers arrive as float64 from
// well-behaved callers, but LLM clients frequently emit numeric strings (e.g.
// "50"). Both forms are handled; unknown types fall back to def.
func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
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
func floatArg(args map[string]any, key string, def float64) float64 {
	switch v := args[key].(type) {
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

func strSliceArg(args map[string]any, key string) []string {
	raw, ok := args[key]
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
	if a.principal != nil {
		ctx = a.verifiedCallerContext(ctx)
		return WithSessionID(ctx, a.principal.SessionID)
	}
	ctx = callcontext.WithClaimedSession(ctx, a.SessionID)
	ctx = a.withCallerContext(ctx)
	if snapshot, ok := callcontext.FromContext(ctx); ok && snapshot.PrincipalID != "" {
		return WithSessionID(ctx, snapshot.SessionID)
	}
	if a.SessionID == "" {
		return ctx
	}
	return WithSessionID(ctx, a.SessionID)
}

// Native tools that neither forward nor log need no daemon attribution lookup.
func (a *Adapter) withClaimedSessionID(ctx context.Context) context.Context {
	if a.principal != nil {
		return a.withSessionID(ctx)
	}
	ctx = callcontext.WithClaimedSession(ctx, a.SessionID)
	if snapshot, ok := callcontext.FromContext(ctx); ok && snapshot.PrincipalID != "" {
		return WithSessionID(ctx, snapshot.SessionID)
	}
	return WithSessionID(ctx, a.SessionID)
}

// SetRefAttacher wires where extracted session refs are written. Separate from
// the constructors because extraction is opt-in and only the `tether mcp` command
// has the daemon client to supply.
func (a *Adapter) SetRefAttacher(r refAttacher) { a.refs = r }

// SetRefResolver wires the resolver for key-only ref captures.
func (a *Adapter) SetRefResolver(r RefResolver) { a.resolver = r }

// logger returns the adapter's logger or slog's default, matching addTool.
func (a *Adapter) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}
