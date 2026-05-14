package mcpadapter

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
)

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

	// Start pool concurrently — failed upstreams are logged but do not abort.
	if err := pool.Start(ctx); err != nil {
		return fmt.Errorf("client pool start: %w", err)
	}
	defer pool.Shutdown()

	// Wire LoggingMiddleware when a Bus is provided.
	var mws []ToolCallMiddleware
	if opts.Bus != nil {
		mws = append(mws, NewLoggingMiddleware(opts.Bus))
	}

	// Subscribe EventStore to Bus so it receives tool_call_end events.
	if opts.Bus != nil && opts.EventStore != nil {
		opts.EventStore.Subscribe(ctx, opts.Bus)
	}

	s := server.NewMCPServer(
		"agent-mux",
		version,
		server.WithToolCapabilities(true),
	)

	// Register LoggingMiddleware as a server-level tool handler middleware so
	// that ALL tool calls — native mux tools and proxied upstream tools alike —
	// emit tool_call_start / tool_call_end events. This covers native tools
	// (mux_health, mux_session_list, mux_message_*, etc.) which previously
	// bypassed the ProxyRouter and were never recorded.
	//
	// Because the server-level middleware now observes every call, we build a
	// plain router (no middleware) for upstream dispatch to avoid double-logging.
	if len(mws) > 0 {
		s.Use(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
			chain := buildMiddlewareChain(ToolCallHandler(next), mws)
			return server.ToolHandlerFunc(chain)
		})
	}

	// Plain router — no middleware; observation is handled server-side above.
	plainRouter := NewProxyRouter(registry)

	// Register native mux tools first (always present regardless of mode).
	a.registerTools(s)

	// Build discovery index — used in all proxy modes (always register mux_discover + mux_call
	// so undeclared servers remain reachable as a safety hatch).
	serverTags := make(map[string][]string, len(entries))
	for _, e := range entries {
		serverTags[e.ID] = e.Tags
	}
	idx := NewDiscoveryIndex()
	idx.Build(registry, serverTags)
	slog.Info("mcp-proxy: discovery index built", "indexed_tools", idx.Len())

	// Build allowed-server set. Empty = all servers (firehose).
	// Threaded into mux_discover and mux_catalog_list_mcp_servers so their
	// responses can mark which tools/servers are reachable directly vs. only
	// via mux_call.
	allowed := make(map[string]struct{}, len(opts.ServerFilter))
	for _, id := range opts.ServerFilter {
		allowed[id] = struct{}{}
	}
	firehose := len(allowed) == 0

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

		for _, def := range registry.AllDefinitions() {
			def := def // capture loop var
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
			a.addTool(s, def, func(handlerCtx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return plainRouter.Handle(handlerCtx, req)
			})
		}

		// Always register mux_discover + mux_call as a safety hatch so agents
		// can reach undeclared servers without needing a restart.
		a.registerDiscoverTool(s, idx, allowed, firehose)
		a.registerCallTool(s, plainRouter)
	}

	// Introspection tool — always registered in proxy mode.
	a.registerMCPServersTool(s, pool, entries, allowed, firehose)

	// Register mux_events_tool_calls when a durable proxy store is wired.
	// Falls back to EventStore for backwards compatibility when ProxyStore
	// is not set (e.g. tests that only wire the in-memory store).
	switch {
	case opts.ProxyStore != nil:
		a.registerToolCallEventsTool(s, opts.ProxyStore)
	case opts.EventStore != nil:
		a.registerToolCallEventsTool(s, &toolCallEventStoreQuerier{store: opts.EventStore})
	}

	ctxFunc := func(_ context.Context) context.Context { return ctx }
	return server.ServeStdio(s, server.WithStdioContextFunc(ctxFunc))
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
func (a *Adapter) registerDiscoverTool(s *server.MCPServer, idx *DiscoveryIndex, nativeServers map[string]struct{}, firehose bool) {
	isNative := func(serverID string) bool {
		if firehose {
			return true
		}
		_, ok := nativeServers[serverID]
		return ok
	}

	a.addTool(s,
		mcp.NewTool("mux_discover",
			mcp.WithDescription(
				"Search the upstream tool catalog by intent, category, or tags. "+
					"Returns matching tool names, descriptions, input schemas, and a `native` flag.\n\n"+
					"How to use the result:\n"+
					"  • If a result has `native: true`, the tool is already in your tool list — "+
					"call it directly by its `tool_name` (do NOT wrap it in mux_call).\n"+
					"  • If a result has `native: false`, the tool is reachable only via "+
					"mux_call(tool_name, arguments).\n"+
					"  • If the response includes `truncated: true`, narrow your query (more "+
					"specific intent/category/tags) or raise `limit` (max 50).\n\n"+
					"Examples:\n"+
					"  mux_discover(intent=\"create a task\")\n"+
					"  mux_discover(category=\"memory\")\n"+
					"  mux_discover(intent=\"list sessions\", limit=20)",
			),
			mcp.WithString("intent",
				mcp.Description("Free-text description of what you want to do (e.g. 'create a sprint', 'run a blueprint')"),
			),
			mcp.WithString("category",
				mcp.Description("Exact category/tag to filter by (e.g. 'tasks', 'automation', 'memory', 'services')"),
			),
			mcp.WithString("tags",
				mcp.Description("Comma-separated additional tag filters (AND semantics)"),
			),
			mcp.WithString("limit",
				mcp.Description("Max tools to return (default 10, max 50)"),
			),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			intent := str(req, "intent")
			category := str(req, "category")
			tagsRaw := str(req, "tags")
			limit := intArg(req, "limit", 10)

			var extraTags []string
			for _, t := range strings.Split(tagsRaw, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					extraTags = append(extraTags, t)
				}
			}

			results, totalMatches := idx.Search(intent, category, extraTags, limit)

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
			return toolJSON(payload), nil
		},
	)
}

// registerCallTool registers mux_call on s. It accepts a tool name and
// arguments object, looks the tool up in the registry, and forwards it
// through the ProxyRouter. Observation/logging happens at the server
// middleware layer (via s.Use()), not inside the router — all proxied calls
// flow here so they are recorded in the event store.
func (a *Adapter) registerCallTool(s *server.MCPServer, router *ProxyRouter) {
	a.addTool(s,
		mcp.NewTool("mux_call",
			mcp.WithDescription(
				"Fallback dispatcher for upstream MCP tools that are NOT in your native tool list. "+
					"If the tool you need already appears in your tool list (e.g. memory_recall, "+
					"clockwork_task_create), call it directly — do NOT wrap it in mux_call.\n\n"+
					"Use mux_call only when:\n"+
					"  • A tool's `native: false` flag was returned by mux_discover, OR\n"+
					"  • You need a tool from a server outside the current --servers filter.\n\n"+
					"Run mux_discover first if you don't know the exact tool name or input schema. "+
					"Arguments must match the tool's input schema exactly.\n\n"+
					"Example:\n"+
					"  mux_call(tool_name=\"some_unlisted_tool\", arguments={\"key\":\"value\"})",
			),
			mcp.WithString("tool_name",
				mcp.Required(),
				mcp.Description("The exact tool name to call (as returned by mux_discover)"),
			),
			mcp.WithObject("arguments",
				mcp.Description("Arguments object matching the tool's input schema"),
			),
		),
		func(handlerCtx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			toolName := str(req, "tool_name")
			if toolName == "" {
				return toolError("invalid_request", "tool_name is required"), nil
			}

			// Extract the arguments sub-object.
			var args map[string]any
			if raw, ok := req.GetArguments()["arguments"]; ok {
				switch v := raw.(type) {
				case map[string]any:
					args = v
				default:
					return toolError("invalid_request", "arguments must be a JSON object"), nil
				}
			}
			if args == nil {
				args = map[string]any{}
			}

			// Build a forwarding CallToolRequest under the target tool name.
			forwarded := mcp.CallToolRequest{}
			forwarded.Params.Name = toolName
			forwarded.Params.Arguments = args

			return router.Handle(handlerCtx, forwarded)
		},
	)
}

// registerMCPServersTool adds the mux_catalog_list_mcp_servers native tool to s.
// It uses the pool for live status and the original entries slice for disabled entries.
//
// nativeServers is the set of server IDs whose tools were registered natively
// at startup. firehose=true means every server is native. Each server entry in
// the response carries `surface: "native_flat"` (call tools directly) or
// `surface: "proxy_only"` (only reachable via mux_discover/mux_call).
func (a *Adapter) registerMCPServersTool(s *server.MCPServer, pool *ClientPool, allEntries []config.MCPServerEntry, nativeServers map[string]struct{}, firehose bool) {
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

	a.addTool(s,
		mcp.NewTool(
			"mux_catalog_list_mcp_servers",
			mcp.WithDescription(
				"List all upstream MCP servers configured in the agent-mux catalog.\n\n"+
					"Each server reports `surface`:\n"+
					"  • \"native_flat\" — this server's tools are in your tool list; call them directly.\n"+
					"  • \"proxy_only\"  — this server's tools are reachable only via mux_discover + mux_call.\n"+
					"  • \"disabled\"    — server is configured but not connected.\n\n"+
					"Use mux_discover to search the catalog by intent/category when you don't know a tool name.",
			),
		),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	)
}
