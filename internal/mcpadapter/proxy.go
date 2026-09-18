package mcpadapter

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ToolCall is one proxied tool invocation, decoded from the native server's
// go-mcp ToolHandler arguments -- (toolName, args, meta) rather than a raw
// wire request, since the native handler is where go-mcp already decoded
// the incoming call.
type ToolCall struct {
	ToolName string
	Args     map[string]any
	Meta     map[string]any
}

// ToolCallHandler is the terminal handler in a middleware chain. It returns
// the raw upstream result -- *mcpsdk.CallToolResult, not go-mcp's simplified
// (any, error) native-tool contract -- because a proxied call must preserve
// arbitrary upstream content (images, multiple content blocks, IsError)
// verbatim rather than being re-marshaled through go-mcp's own opinionated
// JSON envelope. See addProxyTools in proxy_adapter.go, which registers
// proxy tools directly against the underlying official-SDK server for
// exactly this reason.
type ToolCallHandler func(ctx context.Context, call ToolCall) (*mcpsdk.CallToolResult, error)

// ToolCallMiddleware wraps a ToolCallHandler for cross-cutting concerns such
// as logging, tracing, and rate limiting. Implementations are defined in
// middleware_logging.go and future files. See ADR 0021.
type ToolCallMiddleware interface {
	Handle(ctx context.Context, call ToolCall, next ToolCallHandler) (*mcpsdk.CallToolResult, error)
}

// ProxyRouter receives tools/call requests and forwards them to the correct
// upstream client via the ToolRegistry. Native tools must not reach this
// router — they are dispatched by the MCP server handler before calling Handle.
type ProxyRouter struct {
	pool               *ClientPool
	registry           *ToolRegistry
	middleware         []ToolCallMiddleware
	workstreamResolver WorkstreamResolver
	logger             *slog.Logger
}

// NewProxyRouter creates a router backed by the given registry with no middleware.
func NewProxyRouter(registry *ToolRegistry) *ProxyRouter {
	return &ProxyRouter{registry: registry}
}

// NewProxyRouterWithMiddleware creates a router with the given middleware chain.
// Middleware is applied in slice order: index 0 is outermost (runs first).
func NewProxyRouterWithMiddleware(registry *ToolRegistry, mws ...ToolCallMiddleware) *ProxyRouter {
	return &ProxyRouter{registry: registry, middleware: mws}
}

// AppendMiddleware appends mws to the router's middleware chain.
func (r *ProxyRouter) AppendMiddleware(mws ...ToolCallMiddleware) {
	r.middleware = append(r.middleware, mws...)
}

// Handle dispatches a proxied tool call through the middleware chain to the
// appropriate upstream client.
//
// Return semantics (per MCP spec):
//   - Tool-level errors (dead upstream, tool not found) → *CallToolResult with IsError:true
//   - Transport/RPC errors (network failure) → non-nil error return
func (r *ProxyRouter) Handle(ctx context.Context, call ToolCall) (*mcpsdk.CallToolResult, error) {
	rt, found := r.registry.Lookup(call.ToolName)
	if !found {
		return errorResult(fmt.Sprintf("tool %q not found in registry", call.ToolName)), nil
	}

	// Native tool reached the proxy — this is a wiring bug in the caller.
	if rt.ServerID == "" {
		return nil, fmt.Errorf("internal: native tool %q must not be routed through ProxyRouter", call.ToolName)
	}

	// Inject server ID into context so middleware can read it without
	// needing to look up the registry again.
	ctx = WithServerID(ctx, rt.ServerID)

	// Record WHICH upstream on the span. This is the dimension that makes a
	// proxied span worth creating at all -- without it, Torque latency cannot
	// be separated from Tesseract latency, and the tool name is not a reliable
	// substitute (proxy_events records server "mux" for Torque tools today, so
	// name-to-server is already not a mapping anything should lean on).
	//
	// Set here rather than at span creation because this is the one place the
	// registry lookup has already happened; doing it in addProxyTools would
	// mean a second lookup and a second thing that can disagree. A no-op when
	// no span is recording, so this is safe on every path.
	//
	// mux_call also reaches Handle, and its span (created natively by addTool)
	// picks the attribute up here too. That is intended: mux_call forwards to
	// an upstream as well and should carry which one.
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("hollis.tool.server", rt.ServerID))

	// Terminal handler: forward to upstream.
	terminal := ToolCallHandler(func(tCtx context.Context, tCall ToolCall) (*mcpsdk.CallToolResult, error) {
		// Upstream client is nil — server failed during startup or is dead.
		if rt.Client == nil {
			return errorResult(fmt.Sprintf(
				"upstream server %q is unavailable; tool %q cannot be called",
				rt.ServerID, tCall.ToolName,
			)), nil
		}
		if r.pool != nil {
			if err := r.pool.unavailableError(rt.ServerID, rt.Client); err != nil {
				return errorResult(err.Error()), nil
			}
		}
		params := &mcpsdk.CallToolParams{Name: tCall.ToolName, Arguments: tCall.Args, Meta: mcpsdk.Meta(stripSDKMeta(tCall.Meta))}
		r.applyProvenanceMeta(tCtx, params)
		// Trace context rides in params._meta, never in params.arguments --
		// an upstream with additionalProperties:false at its schema root
		// correctly rejects an argument it did not declare. See trace_meta.go.
		injectTraceContextMeta(tCtx, params)
		result, err := rt.Client.CallTool(tCtx, params)
		if err != nil && r.pool != nil {
			return nil, fmt.Errorf("upstream %q call failed; execution outcome may be unknown; request was not replayed: %w", rt.ServerID, err)
		}
		return result, err
	})

	// Apply middleware chain (if any) around the terminal handler.
	chain := buildMiddlewareChain(terminal, r.middleware)
	return chain(ctx, call)
}

// errorResult builds a *mcpsdk.CallToolResult reporting a tool-level failure
// (IsError:true), the raw-SDK equivalent of mark3labs' NewToolResultError.
func errorResult(msg string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: msg}}, IsError: true}
}

// stripSDKMeta removes the official SDK's own reserved per-call metadata keys
// (io.modelcontextprotocol/protocolVersion, /clientInfo, /clientCapabilities,
// and siblings) before a call is forwarded to an upstream.
//
// Under the stateless (2026-07-28) protocol, a *mcpsdk.ClientSession attaches
// these to EVERY outbound tools/call automatically -- there being no
// persistent session for the server to remember them from -- so they arrive
// on Tether's own inbound call describing the ORIGINAL caller's identity as
// an MCP client of Tether. That is not Tether's identity as an MCP client of
// whatever it calls next: client_pool.go's own connection to the upstream
// attaches its OWN such keys independently, from its own Implementation. Left
// unfiltered, tCall.Meta would relay a foreign caller's protocol-level
// bookkeeping into a wire request to an unrelated server, on top of the
// application-level metadata (trace context, provenance) Tether actually
// intends to forward.
func stripSDKMeta(meta map[string]any) map[string]any {
	if len(meta) == 0 {
		return meta
	}
	var out map[string]any
	for k, v := range meta {
		if strings.HasPrefix(k, "io.modelcontextprotocol/") {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(meta))
		}
		out[k] = v
	}
	return out
}
