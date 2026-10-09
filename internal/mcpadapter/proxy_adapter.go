package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	hotel "github.com/hollis-labs/go-otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/telemetry"
)

// rawProxyHandler wraps fn -- a dispatch from decoded arguments/meta to a raw
// upstream result -- as an official-SDK mcpsdk.ToolHandler, applying the same
// session-ID attachment and trace-span wrapping addTool gives every native
// tool. It is the shared foundation for every tool registered directly
// against the SDK server (bypassing go-mcp's RegisterTool) because it must
// relay an upstream's *mcpsdk.CallToolResult verbatim -- addProxyTools and
// registerCallTool (tether_tool_call), the two paths that forward to a ProxyRouter.
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
		if sc := trace.SpanContextFromContext(extractTraceContext(meta, args)); sc.IsValid() && !telemetry.IsObserved(handlerCtx) {
			handlerCtx = trace.ContextWithRemoteSpanContext(handlerCtx, sc)
		}
		handlerCtx, span := hotel.ToolCallSpan(handlerCtx, spanName)
		defer span.End()

		return fn(handlerCtx, args, meta)
	}
}

type liveProxyCatalog struct {
	mu       sync.Mutex
	adapter  *Adapter
	server   *gomcp.Server
	registry *ToolRegistry
	router   *ProxyRouter
	allowed  map[string]struct{}
	firehose bool
}

func (c *liveProxyCatalog) applyRefresh(refresh ToolRefreshResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(refresh.Delta.Removed) > 0 {
		c.server.SDKServer().RemoveTools(c.filterNativeToolNames(refresh.ServerID, refresh.Delta.Removed)...)
	}

	// SDK AddTool replaces an existing name atomically. Removing updated
	// names first would make accepted tools briefly unknown to live callers.
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
		// made to Torque, Tesseract and Cerberus through `tether mcp --proxy`
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
		// cmd/tether/main.go calls internalotel.Init, so the daemon is fine.
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
			c.adapter.recordProxyRefs(handlerCtx, c.registry, def.Name, args, res, err)
			return res, err
		}))
	}
}

// ProxyOptions configures RunWithProxyOpts behavior for Phase 2+.
type ProxyOptions struct {
	// Bus, when non-nil, enables the LoggingMiddleware that emits
	// tool_call_start / tool_call_end events for every proxied call.
	Bus events.Bus

	// Publisher, when non-nil and Bus is nil, is where the LoggingMiddleware
	// publishes instead: a daemon-only `tether mcp` has no bus of its own and
	// passes a DaemonToolCallPublisher.
	Publisher events.Publisher

	// EventStore, when non-nil, is subscribed to the Bus to accumulate
	// tool call events for the live TUI Activity feed (in-memory ring buffer).
	// See ADR 0021. The TUI reads from this store; the MCP tool
	// tether_events_tool_calls reads from the durable ProxyStore instead.
	EventStore *ToolCallEventStore

	// ProxyStore, when non-nil, is used by tether_events_tool_calls to query the
	// durable proxy_events SQLite table (ADR 0024 §4). When both EventStore
	// and ProxyStore are set, EventStore feeds the TUI and ProxyStore feeds
	// the MCP tool.
	ProxyStore ProxyEventQuerier

	// ModeInputs are resolved once before any upstream starts. The profile
	// tier is a typed hook; selecting/filtering profiles belongs to CW-0008.
	ModeInputs        mcpgateway.ModeInputs
	Profile           mcpgateway.ProfileSelection
	AuthorityProfiles []mcpgateway.ProfileSelection // immutable launch/tool authority floors
	// ServerFilter is a strict upstream restriction in both discovery modes.
	// Nil selects enabled catalog entries; an explicitly empty slice selects none.
	ServerFilter []string
	// Confine also treats a nil filter as an explicit grant of no upstreams.
	Confine bool

	// Only enables curated external-client mode. Only upstream tools from
	// ServerFilter are registered. Native Tether targets are suppressed; gateway infrastructure
	// follows the discovery mode.
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
func proxyLoggingMiddleware(mws []ToolCallMiddleware, registries ...*ToolRegistry) mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			call, ok := req.(*mcpsdk.CallToolRequest)
			if !ok {
				return next(ctx, method, req)
			}
			var args map[string]any
			if len(call.Params.Arguments) > 0 {
				if err := json.Unmarshal(call.Params.Arguments, &args); err != nil {
					args = nil
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
			observed := ToolCall{ToolName: call.Params.Name, Args: args, Meta: map[string]any(call.Params.Meta)}
			if len(registries) > 0 {
				name := call.Params.Name
				if name == "tether_tool_call" {
					name, _ = args["name"].(string)
				}
				if name != "" {
					observed.ToolName = name
				}
				if call.Params.Name == "tether_tool_call" {
					observed.Args, _ = args["arguments"].(map[string]any)
				}
				if target, ok := registries[0].Lookup(name); ok {
					ctx = WithServerID(ctx, target.ServerID)
				}
			}

			result, err := chain(ctx, observed)
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
//  4. Registers the tether_gateway_status introspection tool
//  5. Wires LoggingMiddleware + ToolCallEventStore when opts.Bus is set (ADR 0021)
//
// Without --proxy the caller uses Run and upstream MCP servers are not touched.
func (a *Adapter) RunWithProxyOpts(ctx context.Context, catalogDir string, opts ProxyOptions) error {
	return a.RunWithGatewayOpts(ctx, catalogDir, opts, true)
}

// RunWithGatewayOpts serves the same policy for native-only and proxy clients.
func (a *Adapter) RunWithGatewayOpts(ctx context.Context, catalogDir string, opts ProxyOptions, proxy bool) error {
	if opts.Profile.Profile != nil {
		if err := opts.Profile.Profile.Validate(); err != nil {
			return err
		}
		opts.ModeInputs.Profile = opts.Profile.Profile
	}
	for _, floor := range opts.AuthorityProfiles {
		if floor.Profile == nil {
			return mcpgateway.ErrInvalidSessionMCPPolicy
		}
		if err := floor.Profile.Validate(); err != nil {
			return err
		}
	}
	selection, err := mcpgateway.ResolveMode(opts.ModeInputs)
	if err != nil {
		return err
	}
	registry := NewToolRegistry()
	instructions := ""
	if opts.Profile.Profile != nil {
		instructions = opts.Profile.Profile.Instructions
	}
	s := a.newBareServer(gomcp.WithInstructions(instructions))
	if !proxy && opts.Profile.Profile != nil && opts.Profile.Profile.Servers != nil {
		authored, err := config.LoadMCPServerCatalog(catalogDir)
		if err != nil {
			return err
		}
		if _, err := mcpgateway.SelectOrigins(authoredOriginStates(authored), nil, opts.Profile.Profile); err != nil {
			return err
		}
	}
	var entries []config.MCPServerEntry
	var restrictedOrigins []string
	originOrder := []string{"tether"}
	if proxy {
		authored, loadErr := config.LoadMCPServerCatalog(catalogDir)
		if loadErr != nil {
			return loadErr
		}
		selected := opts.ServerFilter
		if selected == nil && opts.Confine {
			selected = []string{}
		}
		if selected == nil && !opts.Confine {
			selected = []string{}
			for _, entry := range authored {
				if entry.IsEnabled() {
					selected = append(selected, entry.ID)
				}
			}
		}
		restriction := selected
		selected, err = mcpgateway.SelectOrigins(authoredOriginStates(authored), selected, opts.Profile.Profile)
		if err != nil {
			return err
		}
		for _, id := range selected {
			if id == "tether" {
				return reservedOriginError()
			}
		}
		if opts.Profile.Profile != nil && opts.Profile.Profile.Servers != nil {
			for _, id := range opts.Profile.Profile.Servers {
				if id == "tether" {
					continue
				}
				permitted := restriction == nil
				for _, allowed := range restriction {
					if allowed == id {
						permitted = true
					}
				}
				if !permitted {
					restrictedOrigins = append(restrictedOrigins, id)
				}
			}
		}
		var unknown []string
		entries, unknown, err = config.LoadMCPServersConfined(catalogDir, selected)
		if err != nil {
			return fmt.Errorf("load mcp-servers catalog: %w", err)
		}
		if len(unknown) > 0 {
			return fmt.Errorf("unknown or disabled MCP server IDs: %s", strings.Join(unknown, ", "))
		}
	}
	for _, entry := range entries {
		originOrder = append(originOrder, entry.ID)
	}
	if opts.Profile.Profile != nil && opts.Profile.Profile.Servers != nil {
		originOrder = opts.Profile.Profile.Servers
	}
	pool := NewClientPool(entries, registry)
	pool.confineRemote = len(a.protected) > 0 && os.Getenv(config.MCPConfineRemoteEnv) == "1"
	pool.runtime = a.runtime
	a.upstreams = pool
	defer pool.Shutdown()
	var mws []ToolCallMiddleware
	switch {
	case opts.Bus != nil:
		mws = append(mws, NewLoggingMiddleware(opts.Bus).RedactWith(proxyRedactionSet(entries)))
	case opts.Publisher != nil:
		mws = append(mws, NewLoggingMiddleware(opts.Publisher).RedactWith(proxyRedactionSet(entries)))
	}
	for _, mw := range mws {
		if logging, ok := mw.(*LoggingMiddleware); ok {
			logging.contextDecorator = a.withSessionID
			logging.profile, logging.mode = opts.Profile.ID, string(selection.Mode)
		}
	}
	if opts.Bus != nil && opts.EventStore != nil {
		opts.EventStore.Subscribe(ctx, opts.Bus)
	}

	router := NewProxyRouter(registry)
	router.pool = pool
	router.SetLogger(a.logger())
	if a.readsViaDaemon() || (a.svc != nil && a.svc.Store != nil) {
		router.SetWorkstreamResolver(a.sessionWorkstreamID)
	}
	var nativeSession *mcpsdk.ClientSession
	if !opts.Only {
		native := a.newServer()
		a.registerCatalogRefreshTool(native, pool)
		switch {
		case opts.ProxyStore != nil:
			a.registerToolCallEventsTool(native, opts.ProxyStore)
		case opts.EventStore != nil:
			a.registerToolCallEventsTool(native, &toolCallEventStoreQuerier{store: opts.EventStore})
		}
		serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
		serverSession, connectErr := native.SDKServer().Connect(ctx, serverTransport, nil)
		if connectErr != nil {
			return connectErr
		}
		defer func() { _ = serverSession.Close() }()
		nativeSession, err = mcpsdk.NewClient(&mcpsdk.Implementation{Name: "tether-local-dispatch", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
		if err != nil {
			return err
		}
		defer func() { _ = nativeSession.Close() }()
		// List every page: native tools are eligible targets in both modes.
		for page, listErr := range nativeSession.Tools(ctx, nil) {
			if listErr != nil {
				return listErr
			}
			if err := registry.RegisterLocal(page, nativeSession); err != nil {
				return err
			}
		}
	}
	tags := map[string][]string{}
	for _, entry := range entries {
		tags[entry.ID] = entry.Tags
	}
	live := &liveProxyCatalog{adapter: a, server: s, registry: registry, router: router, allowed: map[string]struct{}{}, firehose: selection.Mode == mcpgateway.Flat}
	pool.SetToolRefreshHandler(live.applyRefresh)
	if err := pool.Start(ctx); err != nil {
		return err
	}
	gateway := a.gatewayService(registry, router, selection, tags)
	gateway.Policy = &mcpgateway.Policy{Selection: opts.Profile, Floors: opts.AuthorityProfiles, ServerOrder: originOrder, RestrictedOrigins: restrictedOrigins}
	known := gateway.Snapshot()
	if err := gateway.Policy.ValidateNames(known); err != nil {
		return err
	}
	for _, warning := range gateway.Policy.NameWarnings(known) {
		a.logger().Warn(warning)
	}
	s.SDKServer().AddReceivingMiddleware(gatewaySurfaceMiddleware(gateway))
	if !opts.Only {
		a.registerDocsResources(s)
		s.SDKServer().AddReceivingMiddleware(docsResourceMiddleware(gateway))
	}
	if len(mws) > 0 {
		s.SDKServer().AddReceivingMiddleware(proxyLoggingMiddleware(mws, registry))
	}
	if selection.Mode == mcpgateway.Flat {
		live.addProxyTools(registry.AllDefinitions()...)
	} else {
		a.registerSearchTool(s, gateway)
		a.registerListTool(s, gateway)
		a.registerCallTool(s, gateway)
	}
	a.registerGatewayStatus(s, gateway)
	return s.Run(ctx)
}

func (a *Adapter) registerCatalogRefreshTool(s *gomcp.Server, pool *ClientPool) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_catalog_refresh",
		Description: "Refresh one upstream MCP server's tools/list cache in the running tether process, or all upstreams when no server is specified. " +
			"Use this when an upstream added or removed tools and you want tether to rescan immediately without restarting.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("server", "Optional upstream server ID to refresh. Empty refreshes every connected upstream.", false),
		),
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
	router  *ProxyRouter
	gateway *mcpgateway.Service
}

func (r *routerRefResolver) ResolveRef(ctx context.Context, selector map[string]any) (*ResolvedRef, error) {
	if r.router == nil {
		return nil, errors.New("no proxy router available for ref resolution")
	}
	if r.gateway == nil {
		return nil, errors.New("no gateway policy available for ref resolution")
	}
	name := ""
	var excluded error
	for _, def := range r.router.registry.AllDefinitions() {
		rt, _ := r.router.registry.Lookup(def.Name)
		if rt.UpstreamName == "tesseract_ref_resolve" {
			if _, err := r.gateway.ResolveTarget(def.Name); err != nil {
				excluded = err
				continue
			}
			if name != "" {
				return nil, errors.New("multiple upstream tesseract_ref_resolve tools; ref resolution is ambiguous")
			}
			name = def.Name
		}
	}
	if name == "" {
		if excluded != nil {
			return nil, excluded
		}
		return nil, errors.New("no upstream tesseract_ref_resolve tool available")
	}
	res, err := r.router.Handle(ctx, ToolCall{ToolName: name, Args: selector})
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

func authoredOriginStates(entries []config.MCPServerEntry) map[string]bool {
	out := map[string]bool{"tether": true}
	for _, entry := range entries {
		if entry.ID != "tether" {
			out[entry.ID] = entry.IsEnabled()
		}
	}
	return out
}
