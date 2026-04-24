package mcpadapter

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/events"
)

// ProxyOptions configures RunWithProxy behavior for Phase 2+.
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

// RunWithProxy is identical to Run but additionally:
//  1. Loads MCPServerEntry definitions from <catalogDir>/mcp-servers/
//  2. Starts a ClientPool (spawning stdio subprocesses / SSE connections)
//  3. Registers each upstream tool via AddTool with a ProxyRouter handler
//  4. Registers the mux_catalog_list_mcp_servers introspection tool
//  5. (Phase 2) Wires LoggingMiddleware + ToolCallEventStore when opts.Bus is set
//
// Without --proxy the caller uses Run and upstream MCP servers are not touched.
func (a *Adapter) RunWithProxy(ctx context.Context, catalogDir string) error {
	return a.RunWithProxyOpts(ctx, catalogDir, ProxyOptions{})
}

// RunWithProxyOpts is RunWithProxy with Phase 2 observability and broker options.
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

	if opts.BrokerMode {
		// ── Broker mode (deprecated) ─────────────────────────────────────────
		// Pure discovery: no upstream tools registered natively.
		// Kept for backward compatibility; --servers filtering is preferred.
		slog.Warn("mcp-proxy: --broker is deprecated; use --proxy with optional --servers instead")
		slog.Info("mcp-proxy: broker mode — registering mux_discover + mux_call only",
			"upstream_tools", len(registry.AllDefinitions()))
		a.registerDiscoverTool(s, idx)
		a.registerCallTool(s, plainRouter)
	} else {
		// ── Selective flat mode ───────────────────────────────────────────────
		// Build allowed-server set. Empty = all servers (firehose).
		allowed := make(map[string]struct{}, len(opts.ServerFilter))
		for _, id := range opts.ServerFilter {
			allowed[id] = struct{}{}
		}
		firehose := len(allowed) == 0

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
			s.AddTool(def, func(handlerCtx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return plainRouter.Handle(handlerCtx, req)
			})
		}

		// Always register mux_discover + mux_call as a safety hatch so agents
		// can reach undeclared servers without needing a restart.
		a.registerDiscoverTool(s, idx)
		a.registerCallTool(s, plainRouter)
	}

	// Introspection tool — always registered in proxy mode.
	a.registerMCPServersTool(s, pool, entries)

	// Register mux_events_tool_calls when a durable proxy store is wired.
	// Falls back to EventStore for backwards compatibility when ProxyStore
	// is not set (e.g. tests that only wire the in-memory store).
	switch {
	case opts.ProxyStore != nil:
		registerToolCallEventsTool(s, opts.ProxyStore)
	case opts.EventStore != nil:
		registerToolCallEventsTool(s, &toolCallEventStoreQuerier{store: opts.EventStore})
	}

	ctxFunc := func(_ context.Context) context.Context { return ctx }
	return server.ServeStdio(s, server.WithStdioContextFunc(ctxFunc))
}

// registerDiscoverTool registers mux_discover on s. It lets the LLM search the
// upstream tool catalog by intent, category, or tags without receiving every
// tool schema upfront. Returns up to `limit` matching tool schemas as JSON.
func (a *Adapter) registerDiscoverTool(s *server.MCPServer, idx *DiscoveryIndex) {
	s.AddTool(
		mcp.NewTool("mux_discover",
			mcp.WithDescription(
				"Search the upstream tool catalog by intent, category, or tags. "+
					"Returns matching tool names, descriptions, and input schemas so you can "+
					"construct calls to mux_call. Use this before calling mux_call when you "+
					"don't know the exact tool name or need to explore what's available.\n\n"+
					"Examples:\n"+
					"  mux_discover(intent=\"create a task\") → clockwork task tools\n"+
					"  mux_discover(category=\"automation\") → hadron blueprint tools\n"+
					"  mux_discover(intent=\"list sessions\") → session management tools",
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

			results := idx.Search(intent, category, extraTags, limit)
			return toolJSON(map[string]any{
				"ok":      true,
				"count":   len(results),
				"tools":   results,
				"hint":    "Use mux_call(tool_name, arguments) to execute tools not already in your native tool list.",
			}), nil
		},
	)
}

// registerCallTool registers mux_call on s. It accepts a tool name and
// arguments object, looks the tool up in the registry, and forwards it
// through the ProxyRouter. Observation/logging happens at the server
// middleware layer (via s.Use()), not inside the router — all proxied calls
// flow here so they are recorded in the event store.
func (a *Adapter) registerCallTool(s *server.MCPServer, router *ProxyRouter) {
	s.AddTool(
		mcp.NewTool("mux_call",
			mcp.WithDescription(
				"Execute any upstream MCP tool by name. Use this for tools not natively listed — "+
					"call mux_discover first to find the tool name and input schema. "+
					"Arguments must match the tool's input schema exactly.\n\n"+
					"Example:\n"+
					"  mux_call(tool_name=\"clockwork_task_create\", arguments={\"title\":\"Fix bug\",\"description\":\"...\"})",
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
func (a *Adapter) registerMCPServersTool(s *server.MCPServer, pool *ClientPool, allEntries []config.MCPServerEntry) {
	// Build a set of IDs that are enabled (present in pool).
	enabledIDs := make(map[string]struct{})
	for _, e := range allEntries {
		if e.IsEnabled() {
			enabledIDs[e.ID] = struct{}{}
		}
	}

	s.AddTool(
		mcp.NewTool(
			"mux_catalog_list_mcp_servers",
			mcp.WithDescription(
				"List all upstream MCP servers configured in the agent-mux catalog. "+
					"Returns each server's ID, transport, connection status, tool count, and tags.",
			),
		),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			live := pool.StatusSummary()
			liveByID := make(map[string]ServerStatus, len(live))
			for _, s := range live {
				liveByID[s.ID] = s
			}

			// Merge live status with disabled entries into a uniform ServerStatus slice.
			out := make([]ServerStatus, 0, len(allEntries))
			for _, e := range allEntries {
				if !e.IsEnabled() {
					out = append(out, ServerStatus{
						ID:        e.ID,
						Transport: e.Transport,
						Status:    "disabled",
						Tags:      e.Tags,
					})
					continue
				}
				if ls, ok := liveByID[e.ID]; ok {
					out = append(out, ls)
				}
			}

			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

			return toolJSON(map[string]any{
				"ok":      true,
				"servers": out,
				"count":   len(out),
			}), nil
		},
	)
}
