package mcpadapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

type clientStatus struct {
	entry     config.MCPServerEntry
	client    mcpclient.MCPClient
	toolCount int
	err       error
	state     string
	restarts  int
	nextRetry time.Time
	lastExit  *UpstreamExit
	stderr    string
	exhausted bool
}

type ToolRefreshResult struct {
	ServerID  string
	ToolCount int
	Delta     ToolDelta
}

// RefreshAllError reports per-server failures during a multi-server refresh
// while still allowing successful servers to complete.
type RefreshAllError struct {
	Failures map[string]error
}

func (e *RefreshAllError) Error() string {
	if e == nil || len(e.Failures) == 0 {
		return ""
	}
	parts := make([]string, 0, len(e.Failures))
	for serverID, err := range e.Failures {
		parts = append(parts, fmt.Sprintf("%s: %v", serverID, err))
	}
	sort.Strings(parts)
	return "refresh failures: " + strings.Join(parts, "; ")
}

// ClientPool supervises each stdio leaf independently. RPCs are never replayed.
type ClientPool struct {
	entries            []config.MCPServerEntry
	registry           *ToolRegistry
	mu                 sync.Mutex
	catalogMu          sync.Mutex // serialize registry changes and their publication
	statuses           map[string]*clientStatus
	refreshing         map[mcpclient.MCPClient]bool
	connectFn          func(context.Context, config.MCPServerEntry) (mcpclient.MCPClient, error)
	toolRefreshHandler func(ToolRefreshResult)
	cancel             context.CancelFunc
	workers            sync.WaitGroup
	policy             recoveryPolicy
}

type recoveryPolicy struct {
	delays           []time.Duration
	stableFor        time.Duration
	handshakeTimeout time.Duration
}

func NewClientPool(entries []config.MCPServerEntry, registry *ToolRegistry) *ClientPool {
	return &ClientPool{entries: entries, registry: registry,
		statuses: make(map[string]*clientStatus), refreshing: make(map[mcpclient.MCPClient]bool),
		policy: recoveryPolicy{delays: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}, stableFor: time.Minute, handshakeTimeout: 10 * time.Second}}
}

func (p *ClientPool) SetConnectFunc(fn func(context.Context, config.MCPServerEntry) (mcpclient.MCPClient, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connectFn = fn
}
func (p *ClientPool) SetToolRefreshHandler(fn func(ToolRefreshResult)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.toolRefreshHandler = fn
}

// Start waits for the first bounded handshake per server, then leaves each
// supervisor running until Shutdown. Failed siblings do not abort startup.
func (p *ClientPool) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)
	var initial sync.WaitGroup
	for _, entry := range p.entries {
		if !entry.IsEnabled() {
			continue
		}
		p.mu.Lock()
		p.statuses[entry.ID] = &clientStatus{entry: entry, state: "starting"}
		p.mu.Unlock()
		initial.Add(1)
		p.workers.Add(1)
		go func() { defer p.workers.Done(); p.supervise(ctx, entry, initial.Done) }()
	}
	initial.Wait()
	return nil
}

func (p *ClientPool) supervise(ctx context.Context, entry config.MCPServerEntry, ready func()) {
	var readyOnce sync.Once
	first := true
	defer readyOnce.Do(ready)
	for ctx.Err() == nil {
		p.mu.Lock()
		s := p.statuses[entry.ID]
		s.state = "starting"
		s.nextRetry = time.Time{}
		s.client = nil
		p.mu.Unlock()
		client, err := p.connect(ctx, entry)
		if err == nil {
			p.mu.Lock()
			s.client = client
			p.mu.Unlock()
			client.OnNotification(func(n mcp.JSONRPCNotification) {
				if n.Method == mcp.MethodNotificationToolsListChanged {
					go func() { _, _ = p.refreshServer(ctx, entry.ID, client, "notification") }()
				}
			})
			initCtx, cancel := context.WithTimeout(ctx, p.policy.handshakeTimeout)
			var result *mcp.ListToolsResult
			_, err = client.Initialize(initCtx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "agent-mux-proxy", Version: version}}})
			if err != nil {
				err = fmt.Errorf("initialize: %w", err)
			} else {
				result, err = client.ListTools(initCtx, mcp.ListToolsRequest{})
				if err != nil {
					err = fmt.Errorf("list tools: %w", err)
				}
			}
			cancel()
			if err == nil {
				_, err = p.publish(ctx, entry.ID, client, result.Tools, !first)
			}
		}
		if err != nil {
			p.fail(entry.ID, client, err)
			if client != nil {
				_ = client.Close()
			}
		}
		readyOnce.Do(ready)
		first = false
		if client != nil {
			if leaf, ok := client.(*stdioUpstream); ok {
				// A transport loss alone is insufficient authority to spawn another
				// process. Report it immediately, then wait for the owner process to exit.
				stable := time.NewTimer(p.policy.stableFor)
				var stableC <-chan time.Time
				if err == nil {
					stableC = stable.C
				}
				lost := leaf.lost
			waitExit:
				for {
					select {
					case <-ctx.Done():
						stable.Stop()
						_ = client.Close()
						return
					case <-stableC:
						p.mu.Lock()
						s.restarts = 0
						p.mu.Unlock()
						stableC = nil
					case <-lost:
						p.fail(entry.ID, client, fmt.Errorf("stdio closed; waiting for upstream process exit"))
						_ = client.Close()
						lost = nil
						stableC = nil
					case <-leaf.done:
						stable.Stop()
						p.mu.Lock()
						exit := leaf.exit
						s.lastExit = &exit
						s.stderr = leaf.stderr.String()
						p.mu.Unlock()
						p.fail(entry.ID, client, fmt.Errorf("upstream exited: %s (code %d, signal %q)", exit.Kind, exit.Code, exit.Signal))
						break waitExit
					}
				}
			} else {
				// HTTP/SSE have no child to respawn. Keep observed startup/refresh
				// failures visible; remote reconnection policy is outside this change.
				<-ctx.Done()
				_ = client.Close()
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		if entry.Transport != "stdio" || s.restarts >= len(p.policy.delays) {
			s.state = "failed"
			s.nextRetry = time.Time{}
			if entry.Transport == "stdio" {
				s.exhausted = true
				s.err = fmt.Errorf("restart limit reached (%d); correct the upstream and start a new proxy: %w", len(p.policy.delays), s.err)
			}
			p.mu.Unlock()
			slog.Error("mcp-proxy: upstream recovery stopped", "server", entry.ID, "restart_limit", len(p.policy.delays))
			return
		}
		delay := p.policy.delays[s.restarts]
		s.state = "reconnecting"
		s.nextRetry = time.Now().UTC().Add(delay)
		p.mu.Unlock()
		slog.Warn("mcp-proxy: upstream restart scheduled", "server", entry.ID, "delay", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		s.restarts++
		p.mu.Unlock()
	}
}

// publish rejects results from old connections and canceled operations. A
// successful handshake replaces the client even when tool schemas are equal.
func (p *ClientPool) publish(ctx context.Context, id string, client mcpclient.MCPClient, tools []mcp.Tool, notify bool) (ToolRefreshResult, error) {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	p.mu.Lock()
	s := p.statuses[id]
	if ctx.Err() != nil || s == nil || s.client != client {
		p.mu.Unlock()
		return ToolRefreshResult{}, fmt.Errorf("upstream connection changed or operation canceled")
	}
	if leaf, ok := client.(*stdioUpstream); ok {
		select {
		case <-leaf.lost:
			p.mu.Unlock()
			return ToolRefreshResult{}, fmt.Errorf("upstream transport closed")
		default:
		}
	}
	delta := p.registry.ReplaceServer(id, client, tools)
	s.toolCount = len(tools)
	s.err = nil
	s.state = "connected"
	s.nextRetry = time.Time{}
	handler := p.toolRefreshHandler
	p.mu.Unlock()
	refresh := ToolRefreshResult{ServerID: id, ToolCount: len(tools), Delta: delta}
	if notify && handler != nil {
		handler(refresh)
	}
	slog.Info("mcp-proxy: upstream tools ready", "server", id, "tools", len(tools))
	return refresh, nil
}

func (p *ClientPool) fail(id string, client mcpclient.MCPClient, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.statuses[id]; s != nil && s.client == client && s.state != "reconnecting" && !s.exhausted {
		s.err = err
		s.state = "failed"
		slog.Warn("mcp-proxy: upstream unavailable", "server", id, "err", err)
	}
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
		return newStdioUpstream(ctx, entry)
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
	case "http":
		// Streamable HTTP. Each request is an ordinary POST, so there is no
		// long-lived stream to lose when the upstream restarts — which is the
		// property "sse" lacks and the reason this case exists.
		if entry.URL == "" {
			return nil, fmt.Errorf("http transport requires url")
		}
		opts := []transport.StreamableHTTPCOption{}
		if entry.Token != "" {
			// transport.WithHTTPHeaders, not mcpclient.WithHeaders: the latter
			// returns a transport.ClientOption, which is SSE-only.
			opts = append(opts, transport.WithHTTPHeaders(map[string]string{
				"Authorization": "Bearer " + entry.Token,
			}))
		}
		c, err := mcpclient.NewStreamableHttpClient(entry.URL, opts...)
		if err != nil {
			return nil, err
		}
		// Start opens no connection for this transport, but it is what installs
		// the transport's notification handler — without it the OnNotification
		// hook in startOne never sees tools/list_changed.
		if err := c.Start(ctx); err != nil {
			return nil, fmt.Errorf("start http transport: %w", err)
		}
		return c, nil
	default:
		return nil, fmt.Errorf("unknown transport %q (want stdio, sse, or http)", entry.Transport)
	}
}

func (p *ClientPool) RefreshServer(ctx context.Context, id string) (ToolRefreshResult, error) {
	p.mu.Lock()
	s := p.statuses[id]
	if s == nil {
		p.mu.Unlock()
		return ToolRefreshResult{}, fmt.Errorf("unknown upstream server %q", id)
	}
	client, state := s.client, s.state
	p.mu.Unlock()
	if client == nil || state == "starting" || state == "reconnecting" {
		return ToolRefreshResult{}, fmt.Errorf("upstream %q unavailable (%s)", id, state)
	}
	return p.refreshServer(ctx, id, client, "manual")
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
	failures := make(map[string]error)
	for _, id := range ids {
		res, err := p.RefreshServer(ctx, id)
		if err != nil {
			failures[id] = err
			continue
		}
		out = append(out, res)
	}
	if len(failures) > 0 {
		return out, &RefreshAllError{Failures: failures}
	}
	return out, nil
}

func (p *ClientPool) Shutdown() {
	if p.cancel != nil {
		p.cancel()
	}
	p.workers.Wait()
}

// ServerStatus reports observed connection state, not an active health probe.
// ToolCount includes cached definitions; availability is given by Status.
type ServerStatus struct {
	ID                string        `json:"id"`
	Transport         string        `json:"transport"`
	Status            string        `json:"status"`
	Error             string        `json:"error,omitempty"`
	ToolCount         int           `json:"tool_count"`
	Tags              []string      `json:"tags,omitempty"`
	RestartAttempts   int           `json:"restart_attempts"`
	RestartLimit      int           `json:"restart_limit"`
	NextRetryAt       *time.Time    `json:"next_retry_at,omitempty"`
	LastExit          *UpstreamExit `json:"last_exit,omitempty"`
	StderrTail        string        `json:"stderr_tail,omitempty"`
	RecoveryExhausted bool          `json:"recovery_exhausted"`
}

func (p *ClientPool) StatusSummary() []ServerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ServerStatus, 0, len(p.statuses))
	for _, s := range p.statuses {
		ss := ServerStatus{ID: s.entry.ID, Transport: s.entry.Transport, Tags: s.entry.Tags, ToolCount: s.toolCount, Status: s.state, RestartAttempts: s.restarts, LastExit: s.lastExit, StderrTail: s.stderr}
		ss.RecoveryExhausted = s.exhausted
		if s.entry.Transport == "stdio" {
			ss.RestartLimit = len(p.policy.delays)
		}
		if s.err != nil {
			ss.Error = s.err.Error()
		}
		if !s.nextRetry.IsZero() {
			next := s.nextRetry
			ss.NextRetryAt = &next
		}
		if leaf, ok := s.client.(*stdioUpstream); ok {
			if tail := leaf.stderr.String(); tail != "" {
				ss.StderrTail = tail
			}
		}
		out = append(out, ss)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *ClientPool) refreshServer(ctx context.Context, id string, client mcpclient.MCPClient, source string) (ToolRefreshResult, error) {
	callerCtx := ctx
	p.mu.Lock()
	if p.refreshing[client] {
		p.mu.Unlock()
		return ToolRefreshResult{}, fmt.Errorf("upstream %q refresh already running", id)
	}
	p.refreshing[client] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.refreshing, client); p.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, p.policy.handshakeTimeout)
	defer cancel()
	result, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		callerErr := callerCtx.Err()
		if callerErr == nil || !errors.Is(err, callerErr) {
			p.fail(id, client, fmt.Errorf("refresh list tools (%s): %w", source, err))
		}
		return ToolRefreshResult{}, err
	}
	return p.publish(ctx, id, client, result.Tools, true)
}
