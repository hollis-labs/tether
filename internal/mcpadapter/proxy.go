package mcpadapter

import (
	"context"
	"fmt"

	otelprop "github.com/hollis-labs/go-otel/propagation"
	"github.com/mark3labs/mcp-go/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ToolCallHandler is the terminal handler in a middleware chain. It receives
// the original MCP call request and returns the upstream result.
type ToolCallHandler func(ctx context.Context, call mcp.CallToolRequest) (*mcp.CallToolResult, error)

// ToolCallMiddleware wraps a ToolCallHandler for cross-cutting concerns such
// as logging, tracing, and rate limiting. Implementations are defined in
// middleware_logging.go and future files. See ADR 0021.
type ToolCallMiddleware interface {
	Handle(ctx context.Context, call mcp.CallToolRequest, next ToolCallHandler) (*mcp.CallToolResult, error)
}

// ProxyRouter receives tools/call requests and forwards them to the correct
// upstream client via the ToolRegistry. Native tools must not reach this
// router — they are dispatched by the MCP server handler before calling Handle.
type ProxyRouter struct {
	registry   *ToolRegistry
	middleware []ToolCallMiddleware
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
func (r *ProxyRouter) Handle(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.Params.Name

	rt, found := r.registry.Lookup(name)
	if !found {
		return mcp.NewToolResultError(fmt.Sprintf("tool %q not found in registry", name)), nil
	}

	// Native tool reached the proxy — this is a wiring bug in the caller.
	if rt.ServerID == "" {
		return nil, fmt.Errorf("internal: native tool %q must not be routed through ProxyRouter", name)
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
	terminal := ToolCallHandler(func(tCtx context.Context, tReq mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Upstream client is nil — server failed during startup or is dead.
		if rt.Client == nil {
			return mcp.NewToolResultError(fmt.Sprintf(
				"upstream server %q is unavailable; tool %q cannot be called",
				rt.ServerID, tReq.Params.Name,
			)), nil
		}
		if args, ok := tReq.Params.Arguments.(map[string]any); ok || tReq.Params.Arguments == nil {
			tReq.Params.Arguments = otelprop.InjectMCP(tCtx, args)
		}
		return rt.Client.CallTool(tCtx, tReq)
	})

	// Apply middleware chain (if any) around the terminal handler.
	chain := buildMiddlewareChain(terminal, r.middleware)
	return chain(ctx, req)
}
