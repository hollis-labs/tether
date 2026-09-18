package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	hotel "github.com/hollis-labs/go-otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
)

// rawProxyHandler wraps fn -- a dispatch from decoded arguments/meta to a raw
// upstream result -- as an official-SDK mcpsdk.ToolHandler, applying the same
// session-ID attachment and trace-span wrapping addTool gives every native
// tool. It is the shared foundation for every tool registered directly
// against the SDK server (bypassing go-mcp's RegisterTool) because it must
// relay an upstream's *mcpsdk.CallToolResult verbatim -- addProxyTools and
// registerCallTool (mux_call), the two paths that forward to a ProxyRouter.
func (a *Adapter) rawProxyHandler(spanName string, fn func(ctx context.Context, args, meta map[string]any) (*mcpsdk.CallToolResult, error)) mcpsdk.ToolHandler {
	return func(handlerCtx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errorResult("invalid arguments: " + err.Error()), nil
			}
		}
		meta := map[string]any(req.Params.Meta)

		handlerCtx = a.withSessionID(handlerCtx)
		if sc := trace.SpanContextFromContext(extractTraceContext(meta, args)); sc.IsValid() {
			handlerCtx = trace.ContextWithRemoteSpanContext(handlerCtx, sc)
		}
		handlerCtx, span := hotel.ToolCallSpan(handlerCtx, spanName)
		defer span.End()

		return fn(handlerCtx, args, meta)
	}
}

type liveProxyCatalog struct {
	mu         sync.Mutex
	adapter    *Adapter
	server     *gomcp.Server
	registry   *ToolRegistry
	router     *ProxyRouter
	index      *DiscoveryIndex
	serverTags map[string][]string
	allowed    map[string]struct{}
	firehose   bool
}

func (c *liveProxyCatalog) applyRefresh(refresh ToolRefreshResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.index.Build(c.registry, c.serverTags)
	if len(refresh.Delta.Removed) > 0 {
		c.server.SDKServer().RemoveTools(c.filterNativeToolNames(refresh.ServerID, refresh.Delta.Removed)...)
	}

	updatedNames := make([]string, 0, len(refresh.Delta.Updated))
	for _, def := range refresh.Delta.Updated {
		updatedNames = append(updatedNames, def.Name)
	}
	if len(updatedNames) > 0 {
		c.server.SDKServer().RemoveTools(c.filterNativeToolNames(refresh.ServerID, updatedNames)...)
	}

	c.addProxyTools(c.filterNativeTools(refresh.ServerID, refresh.Delta.Added)...)
	c.addProxyTools(c.filterNativeTools(refresh.ServerID, refresh.Delta.Updated)...)
}

func (c *liveProxyCatalog) filterNativeTools(serverID string, defs []*mcpsdk.Tool) []*mcpsdk.Tool {
	if !c.firehose {
		if _, ok := c.allowed[serverID]; !ok {
			return nil
		}
	}
	out := make([]*mcpsdk.Tool, 0, len(defs))
	out = append(out, defs...)
	return out
}

func (c *liveProxyCatalog) filterNativeToolNames(serverID string, names []string) []string {
	if !c.firehose {
		if _, ok := c.allowed[serverID]; !ok {
			return nil
		}
	}
	out := make([]string, 0, len(names))
	out = append(out, names...)
	return out
}

// addProxyTools registers defs directly against the underlying official-SDK
// server (c.server.SDKServer().AddTool), NOT through go-mcp's
// RegisterTool/ToolHandler wrapper. A proxied tool's result is arbitrary,
// upstream-declared content (images, multiple content blocks, an upstream's
// own IsError) that must reach the caller byte-for-byte; go-mcp's simplified
// ToolHandler contract (any, error) has no way to say "use exactly this
// pre-built CallToolResult" -- returning one through it would get
// re-marshaled as opaque StructuredContent instead of passed through as
// protocol-level content. def is already a *mcpsdk.Tool (from the registry,
// populated verbatim from the upstream's own tools/list response), including
// whatever annotations that upstream declared or omitted, so it registers
// unchanged -- there is no annotation derivation to do here.
//
// Sanitize protection still applies: it is a global receiving middleware
// (see Adapter.newServer / RunWithProxyOpts), not a per-tool wrapper, so it
// runs ahead of every tools/call dispatch regardless of which registration
// path a tool came through.
func (c *liveProxyCatalog) addProxyTools(defs ...*mcpsdk.Tool) {
	if len(defs) == 0 {
		return
	}
	sdk := c.server.SDKServer()
	for _, def := range defs {
		def := def
		// Span creation mirrors addTool (adapter.go) deliberately: until
		// CW-20260912-0068 this was the ONLY registration path in the
		// package that did not create one, so every call every session
		// made to Torque, Tesseract and Cerberus through `mux mcp --proxy`
		// was absent from tracing.
		//
		// Note what is NOT changed to fix that: proxy.go's InjectMCP call
		// on the forwarded request. It was already there and already
		// running on every proxied call — InjectMCP returns its params
		// untouched when the span context is invalid, and with no span
		// upstream the context never was valid. So the propagation was an
		// active code path with nothing to say, and creating the span here
		// is what gives it something. Injection starts working without the
		// injection line changing.
		//
		// IF YOU ARE HERE BECAUSE TRACE CONTEXT IS STILL NOT REACHING AN
		// UPSTREAM: check that a real TracerProvider is installed before
		// suspecting this code. OpenTelemetry's default global provider is
		// a no-op, and its spans carry an INVALID span context — so
		// ToolCallSpan below succeeds, returns a span, and InjectMCP then
		// correctly writes nothing. The symptom is byte-identical to the
		// bug this block fixed: the upstream receives its arguments with
		// no _traceparent, exactly as it did before the span existed.
		//
		// cmd/mux/main.go calls internalotel.Init, so the daemon is fine.
		// That is precisely why this bites somewhere else — a test, a
		// short-lived tool, an embedding of this package — and looks
		// impossible when it does.
		sdk.AddTool(def, c.adapter.rawProxyHandler(def.Name, func(handlerCtx context.Context, args, meta map[string]any) (*mcpsdk.CallToolResult, error) {
			res, err := c.router.Handle(handlerCtx, ToolCall{ToolName: def.Name, Args: args, Meta: meta})
			// Typed extraction reads the outbound call and result into Tether's
			// own store. It runs after the call so it can gate on
			// success, and it never touches req -- adding anything to the
			// forwarded call is CW-20260912-0024, the opposite direction
			// through this same seam. See extract.go.
			if c.adapter.resolver == nil && c.router != nil {
				c.adapter.resolver = &routerRefResolver{router: c.router}
			}
			c.adapter.recordRefs(handlerCtx, def.Name, args, res, err)
			return res, err
		}))
	}
}

// ProxyOptions configures RunWithProxyOpts behavior for Phase 2+.
type ProxyOptions struct {
	// Bus, when non-nil, enables the LoggingMiddleware that emits
	// tool_call_start / tool_call_end events for every proxied call.
	Bus events.Bus

	// EventStore, when non-nil, is subscribed to the Bus to accumulate
	// tool call events for the live TUI Activity feed (in-memory ring buffer).
	// See ADR 0021. The TUI reads from this store; the MCP tool
	// mux_events_tool_calls reads from the durable ProxyStore instead.
	EventStore *ToolCallEventStore

	// ProxyStore, when non-nil, is used by mux_events_tool_calls to query the
	// durable proxy_events SQLite table (ADR 0024 §4). When both EventStore
	// and ProxyStore are set, EventStore feeds the TUI and ProxyStore feeds
	// the MCP tool.
	ProxyStore ProxyEventQuerier

	// BrokerMode, when true, enables progressive tool discovery instead of
	// registering every upstream tool at startup. Only mux_discover,
	// mux_call, and mux_catalog_list_mcp_servers are registered with the MCP
	// server. The LLM queries mux_discover to receive tool schemas on demand,
	// then calls mux_call to execute them. This reduces per-request context
	// size by an order of magnitude for large upstream catalogs.
	//
	// Deprecated: use ServerFilter instead. BrokerMode is kept for backward
	// compatibility and still works.
	BrokerMode bool

	// ServerFilter, when non-empty, limits which upstream servers are registered
	// as native flat tools. Servers not in the list are still reachable via
	// mux_discover + mux_call. Empty means all servers (firehose). Only
	// consulted when BrokerMode is false.
	ServerFilter []string

	// Only enables curated external-client mode. Only upstream tools from
	// ServerFilter are registered. Native Tether mux_* tools, mux_discover,
	// mux_discover_tools, mux_call, and proxy catalog tools are suppressed.
	Only bool
}

// proxyLoggingMiddleware wraps mws (the ToolCallMiddleware chain, e.g.
// LoggingMiddleware) as a server-level [mcpsdk.Middleware], the raw-SDK
// receiving-middleware hook go-mcp's own sanitize.Middleware also uses (see
// Adapter.newBareServer). Installing it here rather than per-tool is what
// makes it observe EVERY tools/call dispatch uniformly -- native tools
// registered through go-mcp's RegisterTool and proxy tools registered
// directly against the SDK server (addProxyTools) alike -- mirroring
// mark3labs' s.Use, which sat above per-tool handlers the same way.
func proxyLoggingMiddleware(mws []ToolCallMiddleware) mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			call, ok := req.(*mcpsdk.CallToolRequest)
			if !ok {
				return next(ctx, method, req)
			}
			var args map[string]any
			if len(call.Params.Arguments) > 0 {
				if err := json.Unmarshal(call.Params.Arguments, &args); err != nil {
					return next(ctx, method, req)
				}
			}
			terminal := ToolCallHandler(func(tCtx context.Context, _ ToolCall) (*mcpsdk.CallToolResult, error) {
				res, err := next(tCtx, method, req)
				if err != nil {
					return nil, err
				}
				result, ok := res.(*mcpsdk.CallToolResult)
				if !ok {
					return nil, fmt.Errorf("mcp-proxy: unexpected result type %T for tools/call", res)
				}
				return result, nil
			})
			chain := buildMiddlewareChain(terminal, mws)
			result, err := chain(ctx, ToolCall{ToolName: call.Params.Name, Args: args, Meta: map[string]any(call.Params.Meta)})
			if err != nil {
				return nil, err
			}
			return result, nil
		}
	}
}

// RunWithProxyOpts is identical to Run but additionally:
//  1. Loads MCPServerEntry definitions from <catalogDir>/mcp-servers/
//  2. Starts a ClientPool (spawning stdio subprocesses / SSE connections)
//  3. Registers each upstream tool via AddTool with a ProxyRouter handler
//  4. Registers the mux_catalog_list_mcp_servers introspection tool
//  5. Wires LoggingMiddleware + ToolCallEventStore when opts.Bus is set (ADR 0021)
//
// Without --proxy the caller uses Run and upstream MCP servers are not touched.
func (a *Adapter) RunWithProxyOpts(ctx context.Context, catalogDir string, opts ProxyOptions) error {
	entries, err := config.LoadMCPServers(catalogDir)
	if err != nil {
		return fmt.Errorf("load mcp-servers catalog: %w", err)
	}

	registry := NewToolRegistry()
	pool := NewClientPool(entries, registry)
	pool.runtime = a.runtime
	a.upstreams = pool

	// Wire LoggingMiddleware when a Bus is provided.
	var mws []ToolCallMiddleware
	if opts.Bus != nil {
		mws = append(mws, NewLoggingMiddleware(opts.Bus))
	}

	// Subscribe EventStore to Bus so it receives tool_call_end events.
	if opts.Bus != nil && opts.EventStore != nil {
		opts.EventStore.Subscribe(ctx, opts.Bus)
	}

	s := a.newBareServer()

	// Register LoggingMiddleware as a server-level receiving middleware (over
	// every tools/call dispatch, regardless of registration path) so that ALL
	// tool calls — native mux tools and proxied upstream tools alike — emit
	// tool_call_start / tool_call_end events. This covers native tools
	// (mux_health, mux_session_list, mux_message_*, etc.) which previously
	// bypassed the ProxyRouter and were never recorded.
	//
	// Because the server-level middleware now observes every call, we build a
	// plain router (no middleware) for upstream dispatch to avoid double-logging.
	if len(mws) > 0 {
		s.SDKServer().AddReceivingMiddleware(proxyLoggingMiddleware(mws))
	}

	// Plain router — no middleware; observation is handled server-side above.
	plainRouter := NewProxyRouter(registry)
	plainRouter.pool = pool
	plainRouter.SetLogger(a.logger())
	if a.svc != nil && a.svc.Store != nil {
		plainRouter.SetWorkstreamResolver(func(ctx context.Context, sessionID string) (string, error) {
			row, err := a.svc.Store.GetSession(sessionID)
			if err != nil {
				return "", err
			}
			if row.WorkstreamID.Valid {
				return row.WorkstreamID.String, nil
			}
			return "", nil
		})
	}

	if opts.Only && len(opts.ServerFilter) == 0 {
		return fmt.Errorf("curated proxy --only requires a non-empty server filter")
	}
	if opts.Only && opts.BrokerMode {
		return fmt.Errorf("curated proxy --only cannot be combined with broker mode")
	}

	// Register native mux tools unless curated mode asks for only the selected
	// upstream surface.
	if !opts.Only {
		a.registerTools(s)
	}

	// Build discovery index for proxy modes. Normal and broker modes expose it
	// through mux_discover/mux_discover_tools; curated --only mode keeps the
	// index internal so the selected upstream tools are the entire surface.
	serverTags := make(map[string][]string, len(entries))
	for _, e := range entries {
		serverTags[e.ID] = e.Tags
	}
	idx := NewDiscoveryIndex()

	// Build allowed-server set. Empty = all servers (firehose).
	// Threaded into mux_discover and mux_catalog_list_mcp_servers so their
	// responses can mark which tools/servers are reachable directly vs. only
	// via mux_call.
	allowed := make(map[string]struct{}, len(opts.ServerFilter))
	for _, id := range opts.ServerFilter {
		allowed[id] = struct{}{}
	}
	firehose := len(allowed) == 0
	liveCatalog := &liveProxyCatalog{
		adapter:    a,
		server:     s,
		registry:   registry,
		router:     plainRouter,
		index:      idx,
		serverTags: serverTags,
		allowed:    allowed,
		firehose:   firehose && !opts.BrokerMode,
	}
	pool.SetToolRefreshHandler(liveCatalog.applyRefresh)

	// Start pool concurrently — failed upstreams are logged but do not abort.
	if err := pool.Start(ctx); err != nil {
		return fmt.Errorf("client pool start: %w", err)
	}
	defer pool.Shutdown()
	idx.Build(registry, serverTags)
	slog.Info("mcp-proxy: discovery index built", "indexed_tools", idx.Len())

	if opts.BrokerMode {
		// ── Broker mode (deprecated) ─────────────────────────────────────────
		// Pure discovery: no upstream tools registered natively.
		// Kept for backward compatibility; --servers filtering is preferred.
		slog.Warn("mcp-proxy: --broker is deprecated; use --proxy with optional --servers instead")
		slog.Info("mcp-proxy: broker mode — registering mux_discover + mux_call only",
			"upstream_tools", len(registry.AllDefinitions()))
		// In broker mode no upstream tool is registered natively: pass an empty
		// allowed set with firehose=false so every tool reports native=false.
		a.registerDiscoverTool(s, idx, map[string]struct{}{}, false)
		a.registerSemanticDiscoverTool(s, idx, map[string]struct{}{}, false)
		a.registerCallTool(s, plainRouter)
	} else {
		// ── Selective flat mode ───────────────────────────────────────────────
		if firehose {
			slog.Info("mcp-proxy: flat mode (firehose) — registering all upstream tools natively",
				"upstream_tools", len(registry.AllDefinitions()))
		} else {
			slog.Info("mcp-proxy: selective flat mode — filtering upstream tools",
				"servers", opts.ServerFilter,
				"upstream_tools", len(registry.AllDefinitions()))
		}

		proxied := make([]*mcpsdk.Tool, 0)
		for _, def := range registry.AllDefinitions() {
			rt, ok := registry.Lookup(def.Name)
			if !ok || rt.ServerID == "" {
				continue
			}
			if !firehose {
				if _, inFilter := allowed[rt.ServerID]; !inFilter {
					continue
				}
			}
			slog.Debug("mcp-proxy: registering proxied tool", "tool", def.Name, "server", rt.ServerID)
			proxied = append(proxied, def)
		}
		liveCatalog.addProxyTools(proxied...)

		if !opts.Only {
			// Always register mux_discover + mux_call as a safety hatch so agents
			// can reach undeclared servers without needing a restart.
			a.registerDiscoverTool(s, idx, allowed, firehose)
			a.registerSemanticDiscoverTool(s, idx, allowed, firehose)
			a.registerCallTool(s, plainRouter)
		}
	}

	if !opts.Only {
		// Introspection tools are registered in normal proxy modes. Curated
		// --only mode suppresses them so tools/list contains only selected
		// upstream tools.
		a.registerMCPServersTool(s, pool, entries, allowed, firehose)
		a.registerCatalogRefreshTool(s, pool)
	}

	// Register mux_events_tool_calls when a durable proxy store is wired.
	// Falls back to EventStore for backwards compatibility when ProxyStore
	// is not set (e.g. tests that only wire the in-memory store).
	switch {
	case opts.Only:
	case opts.ProxyStore != nil:
		a.registerToolCallEventsTool(s, opts.ProxyStore)
	case opts.EventStore != nil:
		a.registerToolCallEventsTool(s, &toolCallEventStoreQuerier{store: opts.EventStore})
	}

	return s.Run(ctx)
}

// registerDiscoverTool registers mux_discover on s. It lets the LLM search the
// upstream tool catalog by intent, category, or tags without receiving every
// tool schema upfront. Returns up to `limit` matching tool schemas as JSON.
//
// nativeServers is the set of upstream server IDs whose tools were registered
// natively at startup (the --servers filter). When firehose is true, every
// server's tools are native and nativeServers is ignored. The discover handler
// uses these to mark each result with native: bool so the LLM knows whether
// to call the tool directly or wrap it in mux_call.
func (a *Adapter) registerDiscoverTool(s *gomcp.Server, idx *DiscoveryIndex, nativeServers map[string]struct{}, firehose bool) {
	isNative := func(serverID string) bool {
		if firehose {
			return true
		}
		_, ok := nativeServers[serverID]
		return ok
	}

	a.addTool(s, gomcp.Tool{
		Name: "mux_discover",
		Description: "Search the upstream tool catalog by intent, category, or tags. " +
			"Returns matching tool names, descriptions, input schemas, and a `native` flag.\n\n" +
			"How to use the result:\n" +
			"  • If a result has `native: true`, the tool is already in your tool list — " +
			"call it directly by its `tool_name` (do NOT wrap it in mux_call).\n" +
			"  • If a result has `native: false`, the tool is reachable only via " +
			"mux_call(tool_name, arguments).\n" +
			"  • If the response includes `truncated: true`, narrow your query (more " +
			"specific intent/category/tags) or raise `limit` (max 50).\n\n" +
			"Examples:\n" +
			"  mux_discover(intent=\"create a task\")\n" +
			"  mux_discover(category=\"memory\")\n" +
			"  mux_discover(intent=\"list sessions\", limit=20)",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"intent":   strProp("Free-text description of what you want to do (e.g. 'create a sprint', 'run a blueprint')"),
			"category": strProp("Exact category/tag to filter by (e.g. 'tasks', 'automation', 'memory', 'services')"),
			"tags":     strProp("Comma-separated additional tag filters (AND semantics)"),
			"limit":    strProp("Max tools to return (default 10, max 50)"),
		}),
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			intent := str(args, "intent")
			category := str(args, "category")
			tagsRaw := str(args, "tags")
			limit := intArg(args, "limit", 10)

			var extraTags []string
			for _, t := range strings.Split(tagsRaw, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					extraTags = append(extraTags, t)
				}
			}

			statuses := a.upstreamStatus()
			results, totalMatches := idx.search(intent, category, extraTags, limit, unavailableIDs(statuses))

			tools := make([]map[string]any, 0, len(results))
			for _, r := range results {
				tools = append(tools, map[string]any{
					"tool_name":    r.ToolName,
					"server":       r.ServerID,
					"description":  r.Description,
					"tags":         r.Tags,
					"input_schema": r.InputSchema,
					"score":        r.Score,
					"native":       isNative(r.ServerID),
				})
			}

			truncated := totalMatches > len(results)
			payload := map[string]any{
				"ok":                true,
				"count":             len(tools),
				"total_match_count": totalMatches,
				"truncated":         truncated,
				"tools":             tools,
				"hint": "Tools with native: true are in your tool list — call them directly by tool_name. " +
					"Tools with native: false require mux_call(tool_name, arguments).",
			}
			if truncated {
				payload["more_hint"] = fmt.Sprintf(
					"Returned %d of %d matches. Narrow the query or raise `limit` (max 50) to see more.",
					len(tools), totalMatches,
				)
			}
			addAvailability(payload, statuses)
			return toolJSON(payload), nil
		},
	}, Reads("searches the merged tool catalog").OpenWorld())
}

// registerSemanticDiscoverTool registers mux_discover_tools. It is the
// low-token, task-shaped companion to mux_discover: ranked recommendations are
// grouped by server and point callers to schema/detail refs instead of inlining
// full input schemas.
func (a *Adapter) registerSemanticDiscoverTool(s *gomcp.Server, idx *DiscoveryIndex, nativeServers map[string]struct{}, firehose bool) {
	isNative := func(serverID string) bool {
		if firehose {
			return true
		}
		_, ok := nativeServers[serverID]
		return ok
	}

	a.addTool(s, gomcp.Tool{
		Name: "mux_discover_tools",
		Description: "Find upstream tools for a task intent. Returns concise, ranked recommendations grouped by server/domain. " +
			"Use this before mux_discover when you need tool selection help without full schemas.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"intent":   strProp("Free-text description of the task you want to accomplish"),
			"category": strProp("Optional exact category/tag filter such as tasks, automation, memory, or services"),
			"tags":     strProp("Comma-separated additional tag filters (AND semantics)"),
			"limit":    strProp("Max recommendations to return (default 8, max 20)"),
		}, "intent"),
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			intent := str(args, "intent")
			category := str(args, "category")
			tagsRaw := str(args, "tags")
			limit := intArg(args, "limit", 8)
			if limit <= 0 {
				limit = 8
			}
			if limit > 20 {
				limit = 20
			}

			var extraTags []string
			for _, t := range strings.Split(tagsRaw, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					extraTags = append(extraTags, t)
				}
			}

			statuses := a.upstreamStatus()
			results, totalMatches := idx.search(intent, category, extraTags, limit, unavailableIDs(statuses))
			payload := semanticDiscoveryPayload(intent, results, totalMatches, isNative)
			addAvailability(payload, statuses)
			return toolJSON(payload), nil
		},
	}, Reads("searches the merged tool catalog").OpenWorld())
}

func semanticDiscoveryPayload(intent string, results []SearchResult, totalMatches int, isNative func(string) bool) map[string]any {
	type recommendation struct {
		CallName string         `json:"call_name"`
		Server   string         `json:"server"`
		Summary  string         `json:"summary"`
		Tags     []string       `json:"tags,omitempty"`
		Safety   string         `json:"safety"`
		Native   bool           `json:"native"`
		Why      string         `json:"why"`
		Score    int            `json:"score"`
		Refs     map[string]any `json:"refs"`
	}
	type group struct {
		Server          string           `json:"server"`
		Domain          string           `json:"domain"`
		Recommendations []recommendation `json:"recommendations"`
	}

	groupsByServer := make(map[string]*group)
	order := make([]string, 0)
	for _, r := range results {
		g, ok := groupsByServer[r.ServerID]
		if !ok {
			g = &group{
				Server: r.ServerID,
				Domain: semanticDomain(r),
			}
			groupsByServer[r.ServerID] = g
			order = append(order, r.ServerID)
		}
		g.Recommendations = append(g.Recommendations, recommendation{
			CallName: r.ToolName,
			Server:   r.ServerID,
			Summary:  conciseSummary(r.Description),
			Tags:     firstStrings(r.Tags, 3),
			Safety:   inferToolSafety(r),
			Native:   isNative(r.ServerID),
			Why:      recommendationWhy(intent, r),
			Score:    r.Score,
			Refs: map[string]any{
				"schema":   "tools/list:" + r.ToolName,
				"detail":   "mux_discover?intent=" + r.ToolName,
				"catalog":  "mcp-servers/" + r.ServerID,
				"examples": "tool-docs:" + r.ToolName + "#examples",
			},
		})
	}

	groups := make([]group, 0, len(order))
	for _, serverID := range order {
		groups = append(groups, *groupsByServer[serverID])
	}
	return map[string]any{
		"ok":                true,
		"query":             intent,
		"count":             len(results),
		"total_match_count": totalMatches,
		"truncated":         totalMatches > len(results),
		"groups":            groups,
		"hint":              "Call native recommendations directly. For native=false, use mux_call, or use mux_discover for full schemas.",
	}
}

func semanticDomain(r SearchResult) string {
	if len(r.Tags) > 0 && strings.TrimSpace(r.Tags[0]) != "" {
		return r.Tags[0]
	}
	return r.ServerID
}

func conciseSummary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 140 {
		return s
	}
	return strings.TrimSpace(s[:137]) + "..."
}

func firstStrings(in []string, n int) []string {
	if len(in) == 0 || n <= 0 {
		return nil
	}
	if len(in) > n {
		in = in[:n]
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func inferToolSafety(r SearchResult) string {
	text := strings.ToLower(r.ToolName + " " + r.Description)
	for _, word := range []string{"create", "update", "edit", "delete", "remove", "write", "send", "post", "start", "stop", "run", "enqueue"} {
		if strings.Contains(text, word) {
			return "mutating"
		}
	}
	return "read_only"
}

func recommendationWhy(intent string, r SearchResult) string {
	switch {
	case strings.TrimSpace(intent) == "":
		return "Available upstream tool in the matching category."
	case r.Score > 0:
		return fmt.Sprintf("Matched %d intent term(s) against the tool name, description, or tags.", r.Score)
	default:
		return "Matched the requested category or tags."
	}
}

// registerCallTool registers mux_call on s. It accepts a tool name and
// arguments object, looks the tool up in the registry, and forwards it
// through the ProxyRouter.
//
// Registered directly against the SDK server, like addProxyTools, and for
// the identical reason: the upstream's *mcpsdk.CallToolResult must reach the
// caller verbatim, which go-mcp's own (any, error) ToolHandler contract
// cannot represent. Observation/logging happens at the server middleware
// layer (proxyLoggingMiddleware), not inside the router — all proxied calls
// flow here so they are recorded in the event store.
func (a *Adapter) registerCallTool(s *gomcp.Server, router *ProxyRouter) {
	s.SDKServer().AddTool(&mcpsdk.Tool{
		Name: "mux_call",
		Description: "Fallback dispatcher for upstream MCP tools that are NOT in your native tool list. " +
			"If the tool you need already appears in your tool list (e.g. memory_recall, " +
			"clockwork_task_create), call it directly — do NOT wrap it in mux_call.\n\n" +
			"Use mux_call only when:\n" +
			"  • A tool's `native: false` flag was returned by mux_discover, OR\n" +
			"  • You need a tool from a server outside the current --servers filter.\n\n" +
			"Run mux_discover first if you don't know the exact tool name or input schema. " +
			"Arguments must match the tool's input schema exactly.\n\n" +
			"Example:\n" +
			"  mux_call(tool_name=\"some_unlisted_tool\", arguments={\"key\":\"value\"})",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"tool_name": strProp("The exact tool name to call (as returned by mux_discover)"),
			"arguments": objProp("Arguments object matching the tool's input schema"),
		}, "tool_name"),
		Annotations: Writes().OpenWorld().annotations().sdk(),
	}, a.rawProxyHandler("mux_call", func(handlerCtx context.Context, args, meta map[string]any) (*mcpsdk.CallToolResult, error) {
		toolName := str(args, "tool_name")
		if toolName == "" {
			return errorResult("tool_name is required"), nil
		}

		// Extract the arguments sub-object.
		var innerArgs map[string]any
		if raw, ok := args["arguments"]; ok {
			m, ok := raw.(map[string]any)
			if !ok {
				return errorResult("arguments must be a JSON object"), nil
			}
			innerArgs = m
		}
		if innerArgs == nil {
			innerArgs = map[string]any{}
		}

		if a.resolver == nil && router != nil {
			a.resolver = &routerRefResolver{router: router}
		}
		res, err := router.Handle(handlerCtx, ToolCall{ToolName: toolName, Args: innerArgs, Meta: meta})
		a.recordRefs(handlerCtx, toolName, innerArgs, res, err)
		return res, err
	}))
}

// registerMCPServersTool adds the mux_catalog_list_mcp_servers native tool to s.
// It uses the pool for live status and the original entries slice for disabled entries.
//
// nativeServers is the set of server IDs whose tools were registered natively
// at startup. firehose=true means every server is native. Each server entry in
// the response carries `surface: "native_flat"` (call tools directly) or
// `surface: "proxy_only"` (only reachable via mux_discover/mux_call).
func (a *Adapter) registerMCPServersTool(s *gomcp.Server, pool *ClientPool, allEntries []config.MCPServerEntry, nativeServers map[string]struct{}, firehose bool) {
	// Build a set of IDs that are enabled (present in pool).
	enabledIDs := make(map[string]struct{})
	for _, e := range allEntries {
		if e.IsEnabled() {
			enabledIDs[e.ID] = struct{}{}
		}
	}

	surfaceOf := func(serverID string, enabled bool) string {
		if !enabled {
			return "disabled"
		}
		if firehose {
			return "native_flat"
		}
		if _, ok := nativeServers[serverID]; ok {
			return "native_flat"
		}
		return "proxy_only"
	}

	a.addTool(s, gomcp.Tool{
		Name: "mux_catalog_list_mcp_servers",
		Description: "List all upstream MCP servers configured in the agent-mux catalog.\n\n" +
			"Each server reports `surface`:\n" +
			"  • \"native_flat\" — this server's tools are in your tool list; call them directly.\n" +
			"  • \"proxy_only\"  — this server's tools are reachable only via mux_discover + mux_call.\n" +
			"  • \"disabled\"    — server is configured but not connected.\n\n" +
			"Use mux_discover to search the catalog by intent/category when you don't know a tool name.",
		InputSchema: gomcp.EmptyObjectSchema(),
		Handler: func(_ context.Context, _ map[string]any) (any, error) {
			live := pool.StatusSummary()
			liveByID := make(map[string]ServerStatus, len(live))
			for _, s := range live {
				liveByID[s.ID] = s
			}

			// Merge live status with disabled entries into a uniform shape, attaching
			// the surface field per server.
			type serverEntry struct {
				ServerStatus
				Surface string `json:"surface"`
			}
			out := make([]serverEntry, 0, len(allEntries))
			for _, e := range allEntries {
				if !e.IsEnabled() {
					out = append(out, serverEntry{
						ServerStatus: ServerStatus{
							ID:        e.ID,
							Transport: e.Transport,
							Status:    "disabled",
							Tags:      e.Tags,
						},
						Surface: surfaceOf(e.ID, false),
					})
					continue
				}
				if ls, ok := liveByID[e.ID]; ok {
					out = append(out, serverEntry{
						ServerStatus: ls,
						Surface:      surfaceOf(e.ID, true),
					})
				}
			}

			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

			return toolJSON(map[string]any{
				"ok":       true,
				"servers":  out,
				"count":    len(out),
				"firehose": firehose,
				"hint":     "Servers with surface=native_flat have their tools in your tool list — call them directly. Use mux_discover + mux_call for proxy_only servers.",
			}), nil
		},
	}, Reads("configured upstream listing"))
}

func (a *Adapter) registerCatalogRefreshTool(s *gomcp.Server, pool *ClientPool) {
	a.addTool(s, gomcp.Tool{
		Name: "mux_catalog_refresh",
		Description: "Refresh one upstream MCP server's tools/list cache in the running mux process, or all upstreams when no server is specified. " +
			"Use this when an upstream added or removed tools and you want mux to rescan immediately without restarting.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"server": strProp("Optional upstream server ID to refresh. Empty refreshes every connected upstream."),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			serverID := strings.TrimSpace(str(args, "server"))

			var (
				results []ToolRefreshResult
				err     error
			)
			if serverID == "" {
				results, err = pool.RefreshAll(ctx)
			} else {
				var single ToolRefreshResult
				single, err = pool.RefreshServer(ctx, serverID)
				results = []ToolRefreshResult{single}
			}
			var partialErr *RefreshAllError
			if err != nil && !errors.As(err, &partialErr) {
				return nil, toolError("refresh_failed", err.Error())
			}

			items := make([]map[string]any, 0, len(results))
			for _, res := range results {
				items = append(items, map[string]any{
					"server":     res.ServerID,
					"tool_count": res.ToolCount,
					"added":      toolNames(res.Delta.Added),
					"updated":    toolNames(res.Delta.Updated),
					"removed":    res.Delta.Removed,
				})
			}
			body := map[string]any{
				"ok":        true,
				"count":     len(items),
				"refreshed": items,
			}
			if partialErr != nil {
				errorsByServer := make(map[string]string, len(partialErr.Failures))
				for serverID, refreshErr := range partialErr.Failures {
					errorsByServer[serverID] = refreshErr.Error()
				}
				body["partial"] = true
				body["errors"] = errorsByServer
			}
			return toolJSON(body), nil
		},
	}, Writes().OpenWorld())
}

func toolNames(defs []*mcpsdk.Tool) []string {
	out := make([]string, 0, len(defs))
	for _, def := range defs {
		out = append(out, def.Name)
	}
	return out
}

// routerRefResolver implements RefResolver by dispatching to tesseract_ref_resolve
// via the ProxyRouter.
type routerRefResolver struct {
	router *ProxyRouter
}

func (r *routerRefResolver) ResolveRef(ctx context.Context, selector map[string]any) (*ResolvedRef, error) {
	if r.router == nil {
		return nil, errors.New("no proxy router available for ref resolution")
	}
	res, err := r.router.Handle(ctx, ToolCall{ToolName: "tesseract_ref_resolve", Args: selector})
	if err != nil {
		return nil, fmt.Errorf("tesseract_ref_resolve: %w", err)
	}
	if res == nil || res.IsError {
		return nil, errors.New("tesseract_ref_resolve returned tool error")
	}
	m := parseResultMap(res)
	if m == nil {
		return nil, errors.New("tesseract_ref_resolve returned empty or non-JSON result")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out ResolvedRef
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("unmarshal resolved ref: %w", err)
	}
	return &out, nil
}
