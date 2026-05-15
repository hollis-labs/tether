package mcpadapter

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/hollis-labs/tether/internal/config"
)

// clientStatus tracks the runtime state of one upstream client.
type clientStatus struct {
	entry     config.MCPServerEntry
	client    mcpclient.MCPClient
	toolCount int
	err       error // non-nil if startup or ListTools failed
}

// ToolRefreshResult reports one live refresh attempt for an upstream server.
type ToolRefreshResult struct {
	ServerID  string
	ToolCount int
	Delta     ToolDelta
}

// ClientPool manages upstream MCP server connections. It spawns stdio
// subprocesses or connects to SSE endpoints, performs the MCP handshake,
// fetches tool lists, and populates a ToolRegistry.
type ClientPool struct {
	entries  []config.MCPServerEntry
	registry *ToolRegistry

	mu                 sync.Mutex
	statuses           map[string]*clientStatus // serverID → status
	refreshing         map[string]bool
	connectFn          func(context.Context, config.MCPServerEntry) (mcpclient.MCPClient, error)
	toolRefreshHandler func(ToolRefreshResult)
}

// NewClientPool creates a pool from the given catalog entries and registry.
func NewClientPool(entries []config.MCPServerEntry, registry *ToolRegistry) *ClientPool {
	return &ClientPool{
		entries:    entries,
		registry:   registry,
		statuses:   make(map[string]*clientStatus, len(entries)),
		refreshing: make(map[string]bool, len(entries)),
	}
}

// SetConnectFunc overrides the upstream dialer. Used by tests.
func (p *ClientPool) SetConnectFunc(fn func(context.Context, config.MCPServerEntry) (mcpclient.MCPClient, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connectFn = fn
}

// SetToolRefreshHandler configures a callback invoked after a successful
// upstream tool refresh updates the registry.
func (p *ClientPool) SetToolRefreshHandler(handler func(ToolRefreshResult)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.toolRefreshHandler = handler
}

// Start connects to all configured upstream servers concurrently.
// Startup failures for individual servers are logged but do not abort the
// overall start — other servers proceed normally.
func (p *ClientPool) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, entry := range p.entries {
		wg.Add(1)
		go func(e config.MCPServerEntry) {
			defer wg.Done()
			p.startOne(ctx, e)
		}(entry)
	}
	wg.Wait()
	return nil
}

// startOne connects to a single upstream and registers its tools.
func (p *ClientPool) startOne(ctx context.Context, entry config.MCPServerEntry) {
	status := &clientStatus{entry: entry}

	client, err := p.connect(ctx, entry)
	if err != nil {
		status.err = fmt.Errorf("connect: %w", err)
		slog.Error("mcp-proxy: upstream connect failed",
			"server", entry.ID, "transport", entry.Transport, "err", err)
		p.setStatus(entry.ID, status)
		return
	}
	status.client = client
	client.OnNotification(func(notification mcp.JSONRPCNotification) {
		if notification.Method != mcp.MethodNotificationToolsListChanged {
			return
		}
		go p.refreshServer(ctx, entry.ID, client, "notification")
	})

	// MCP initialize handshake.
	_, err = client.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "agent-mux-proxy",
				Version: version,
			},
		},
	})
	if err != nil {
		status.err = fmt.Errorf("initialize: %w", err)
		slog.Error("mcp-proxy: upstream initialize failed",
			"server", entry.ID, "err", err)
		p.setStatus(entry.ID, status)
		return
	}

	// Fetch tool list.
	result, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		status.err = fmt.Errorf("list tools: %w", err)
		slog.Error("mcp-proxy: upstream ListTools failed",
			"server", entry.ID, "err", err)
		p.setStatus(entry.ID, status)
		return
	}

	p.registry.Register(entry.ID, client, result.Tools)
	status.toolCount = len(result.Tools)

	slog.Info("mcp-proxy: upstream connected",
		"server", entry.ID, "transport", entry.Transport, "tools", status.toolCount)
	p.setStatus(entry.ID, status)
}

// connect creates (and starts) the appropriate MCP client for the entry.
// ctx is threaded through so SSE startup cancels promptly on shutdown.
func (p *ClientPool) connect(ctx context.Context, entry config.MCPServerEntry) (mcpclient.MCPClient, error) {
	p.mu.Lock()
	connectFn := p.connectFn
	p.mu.Unlock()
	if connectFn != nil {
		return connectFn(ctx, entry)
	}

	switch entry.Transport {
	case "stdio":
		if entry.Command == "" {
			return nil, fmt.Errorf("stdio transport requires command")
		}
		// Build env slice: "KEY=VALUE" format expected by NewStdioMCPClient.
		env := make([]string, 0, len(entry.Env))
		for k, v := range entry.Env {
			env = append(env, k+"="+v)
		}
		return mcpclient.NewStdioMCPClient(entry.Command, env, entry.Args...)
	case "sse":
		if entry.URL == "" {
			return nil, fmt.Errorf("sse transport requires url")
		}
		opts := []transport.ClientOption{}
		if entry.Token != "" {
			opts = append(opts, mcpclient.WithHeaders(map[string]string{
				"Authorization": "Bearer " + entry.Token,
			}))
		}
		c, err := mcpclient.NewSSEMCPClient(entry.URL, opts...)
		if err != nil {
			return nil, err
		}
		// SSE transport requires an explicit Start call (unlike stdio which
		// auto-starts in NewStdioMCPClient).
		if err := c.Start(ctx); err != nil {
			return nil, fmt.Errorf("start sse transport: %w", err)
		}
		return c, nil
	default:
		return nil, fmt.Errorf("unknown transport %q (want stdio or sse)", entry.Transport)
	}
}

// RefreshServer forces a one-server tools/list refresh without reconnecting the client.
func (p *ClientPool) RefreshServer(ctx context.Context, serverID string) (ToolRefreshResult, error) {
	p.mu.Lock()
	status, ok := p.statuses[serverID]
	p.mu.Unlock()
	if !ok {
		return ToolRefreshResult{}, fmt.Errorf("unknown upstream server %q", serverID)
	}
	if status.client == nil {
		if status.err != nil {
			return ToolRefreshResult{}, fmt.Errorf("upstream %q unavailable: %w", serverID, status.err)
		}
		return ToolRefreshResult{}, fmt.Errorf("upstream %q unavailable", serverID)
	}
	return p.refreshServer(ctx, serverID, status.client, "manual")
}

// RefreshAll forces a tools/list refresh for every connected upstream.
func (p *ClientPool) RefreshAll(ctx context.Context) ([]ToolRefreshResult, error) {
	p.mu.Lock()
	ids := make([]string, 0, len(p.statuses))
	for id := range p.statuses {
		ids = append(ids, id)
	}
	p.mu.Unlock()

	out := make([]ToolRefreshResult, 0, len(ids))
	for _, id := range ids {
		res, err := p.RefreshServer(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// Shutdown terminates all upstream connections gracefully.
func (p *ClientPool) Shutdown() {
	p.mu.Lock()
	statuses := maps.Clone(p.statuses)
	p.mu.Unlock()

	for id, s := range statuses {
		if s.client != nil {
			if err := s.client.Close(); err != nil {
				slog.Warn("mcp-proxy: error closing upstream", "server", id, "err", err)
			}
		}
	}
}

// StatusSummary returns a snapshot of each server's current status.
func (p *ClientPool) StatusSummary() []ServerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]ServerStatus, 0, len(p.statuses))
	for _, s := range p.statuses {
		ss := ServerStatus{
			ID:        s.entry.ID,
			Transport: s.entry.Transport,
			Tags:      s.entry.Tags,
			ToolCount: s.toolCount,
		}
		if s.err != nil {
			ss.Status = "failed"
			ss.Error = s.err.Error()
		} else {
			ss.Status = "connected"
		}
		out = append(out, ss)
	}
	return out
}

// ServerStatus is the public view of one upstream server's runtime state.
type ServerStatus struct {
	ID        string   `json:"id"`
	Transport string   `json:"transport"`
	Status    string   `json:"status"` // "connected" | "failed"
	Error     string   `json:"error,omitempty"`
	ToolCount int      `json:"tool_count"`
	Tags      []string `json:"tags,omitempty"`
}

func (p *ClientPool) setStatus(id string, s *clientStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses[id] = s
}

func (p *ClientPool) refreshServer(ctx context.Context, serverID string, client mcpclient.MCPClient, source string) (ToolRefreshResult, error) {
	if !p.beginRefresh(serverID) {
		return ToolRefreshResult{}, nil
	}
	defer p.endRefresh(serverID)

	result, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		p.setRefreshError(serverID, fmt.Errorf("refresh list tools: %w", err))
		slog.Warn("mcp-proxy: upstream tool refresh failed", "server", serverID, "source", source, "err", err)
		return ToolRefreshResult{}, err
	}

	delta := p.registry.ReplaceServer(serverID, client, result.Tools)
	p.updateToolCount(serverID, len(result.Tools))

	refresh := ToolRefreshResult{
		ServerID:  serverID,
		ToolCount: len(result.Tools),
		Delta:     delta,
	}
	if handler := p.getToolRefreshHandler(); handler != nil {
		handler(refresh)
	}

	slog.Info("mcp-proxy: upstream tools refreshed",
		"server", serverID,
		"source", source,
		"tools", len(result.Tools),
		"added", len(delta.Added),
		"updated", len(delta.Updated),
		"removed", len(delta.Removed))
	return refresh, nil
}

func (p *ClientPool) beginRefresh(serverID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refreshing[serverID] {
		return false
	}
	p.refreshing[serverID] = true
	return true
}

func (p *ClientPool) endRefresh(serverID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.refreshing, serverID)
}

func (p *ClientPool) updateToolCount(serverID string, count int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if status, ok := p.statuses[serverID]; ok {
		status.toolCount = count
		status.err = nil
	}
}

func (p *ClientPool) setRefreshError(serverID string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if status, ok := p.statuses[serverID]; ok {
		status.err = err
	}
}

func (p *ClientPool) getToolRefreshHandler() func(ToolRefreshResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.toolRefreshHandler
}
