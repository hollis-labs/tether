package mcpadapter

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/events"
)

// ProxyOptions configures RunWithProxy behaviour for Phase 2+.
type ProxyOptions struct {
	// Bus, when non-nil, enables the LoggingMiddleware that emits
	// tool_call_start / tool_call_end events for every proxied call.
	Bus events.Bus

	// EventStore, when non-nil, is subscribed to the Bus to accumulate
	// tool call events for the mux_events_tool_calls query tool.
	EventStore *ToolCallEventStore
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

// RunWithProxyOpts is RunWithProxy with Phase 2 observability options.
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

	// Phase 2: wire LoggingMiddleware when a Bus is provided.
	var mws []ToolCallMiddleware
	if opts.Bus != nil {
		mws = append(mws, NewLoggingMiddleware(opts.Bus))
	}

	// Phase 2: subscribe EventStore to Bus so it receives tool_call_end events.
	if opts.Bus != nil && opts.EventStore != nil {
		opts.EventStore.Subscribe(ctx, opts.Bus)
	}

	router := NewProxyRouterWithMiddleware(registry, mws...)

	s := server.NewMCPServer(
		"agent-mux",
		version,
		server.WithToolCapabilities(true),
	)

	// Register native tools first.
	a.registerTools(s)

	// Register each proxied upstream tool with a forwarding handler.
	for _, def := range registry.AllDefinitions() {
		def := def // capture loop var
		rt, ok := registry.Lookup(def.Name)
		if !ok || rt.ServerID == "" {
			// Native tools are already registered above — skip.
			continue
		}
		slog.Debug("mcp-proxy: registering proxied tool", "tool", def.Name, "server", rt.ServerID)
		s.AddTool(def, func(handlerCtx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return router.Handle(handlerCtx, req)
		})
	}

	// Register the mux_catalog_list_mcp_servers introspection tool.
	a.registerMCPServersTool(s, pool, entries)

	// Phase 2: register mux_events_tool_calls when an event store is wired.
	if opts.EventStore != nil {
		registerToolCallEventsTool(s, opts.EventStore)
	}

	ctxFunc := func(_ context.Context) context.Context { return ctx }
	return server.ServeStdio(s, server.WithStdioContextFunc(ctxFunc))
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
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			live := pool.StatusSummary()
			liveByID := make(map[string]ServerStatus, len(live))
			for _, s := range live {
				liveByID[s.ID] = s
			}

			// Merge live status with disabled entries.
			type serverBrief struct {
				ID        string   `json:"id"`
				Transport string   `json:"transport"`
				Status    string   `json:"status"`
				Error     string   `json:"error,omitempty"`
				ToolCount int      `json:"tool_count"`
				Tags      []string `json:"tags,omitempty"`
			}

			out := make([]serverBrief, 0, len(allEntries))
			for _, e := range allEntries {
				if !e.IsEnabled() {
					out = append(out, serverBrief{
						ID:        e.ID,
						Transport: e.Transport,
						Status:    "disabled",
						Tags:      e.Tags,
					})
					continue
				}
				if ls, ok := liveByID[e.ID]; ok {
					out = append(out, serverBrief{
						ID:        ls.ID,
						Transport: ls.Transport,
						Status:    ls.Status,
						Error:     ls.Error,
						ToolCount: ls.ToolCount,
						Tags:      ls.Tags,
					})
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
