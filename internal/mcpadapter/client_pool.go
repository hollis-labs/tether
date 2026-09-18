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

	gomcpclient "github.com/hollis-labs/go-mcp/client"
	"github.com/hollis-labs/go-mcp/supervise"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
)

// upstreamClient is the narrow contract client_pool.go depends on for an
// upstream MCP connection -- satisfied directly by *mcpsdk.ClientSession
// (returned by (*mcpsdk.Client).Connect for the sse/http transports) and by
// *stdioUpstream (which embeds one). Declared narrowly, mirroring Hadron's
// internal_caller.go externalClient, so tests can substitute a fake.
//
// go-mcp has no client package of its own -- it is server-only by design
// (CW-20260918-0013 tracks a possible future shared one, informed by this
// pool as one of its two reference consumers) -- so this drives the official
// SDK's Client/ClientSession directly, per the same "port it directly, don't
// block on a shared package" direction Hadron's port followed.
type upstreamClient interface {
	CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error)
	ListTools(ctx context.Context, params *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error)
	Close() error
}

type clientStatus struct {
	entry      config.MCPServerEntry
	client     upstreamClient
	toolCount  int
	err        error
	state      string
	restarts   int
	nextRetry  time.Time
	lastExit   *supervise.Exit
	stderr     string
	exhausted  bool
	lastLaunch *LaunchObservation
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
	runtime            RuntimeObservation
	entries            []config.MCPServerEntry
	registry           *ToolRegistry
	mu                 sync.Mutex
	catalogMu          sync.Mutex // serialize registry changes and their publication
	statuses           map[string]*clientStatus
	refreshing         map[upstreamClient]bool
	connectFn          func(context.Context, config.MCPServerEntry) (upstreamClient, error)
	toolRefreshHandler func(ToolRefreshResult)
	cancel             context.CancelFunc
	workers            sync.WaitGroup
	policy             recoveryPolicy

	// remoteClients holds sse/http upstream connections, built lazily (see
	// remoteClientPool) so it picks up p.runtime.Build.Version as set by
	// RunWithProxyOpts, which happens after NewClientPool returns. stdio
	// stays on spawnStdioUpstream/mcpsdk.Client directly: go-mcp/client's
	// reactive dial-on-first-use model has no equivalent for the proactive
	// crash-loop backoff, stderr capture, and exit tracking this pool's own
	// supervise loop does for a local subprocess, and stdio is exactly the
	// transport go-mcp/client's own doc singles out Tether as already ahead
	// on. sse/http is the opposite case: Tether's own connect() had no
	// automatic reconnection there at all ("remote reconnection policy is
	// outside this change", below) -- go-mcp/client's lazy health-probed
	// reconnect is a real, uncomplicated gain for exactly that gap.
	remoteClients *gomcpclient.Pool
}

// recoveryPolicy wraps go-mcp/supervise's Policy (backoff schedule +
// stable-for reset window) with handshakeTimeout, a Tether-local knob the
// shared package has no opinion on.
type recoveryPolicy struct {
	supervise.Policy
	handshakeTimeout time.Duration
}

func NewClientPool(entries []config.MCPServerEntry, registry *ToolRegistry) *ClientPool {
	return &ClientPool{runtime: processObservation, entries: entries, registry: registry,
		statuses: make(map[string]*clientStatus), refreshing: make(map[upstreamClient]bool),
		policy: recoveryPolicy{Policy: supervise.DefaultPolicy(), handshakeTimeout: 10 * time.Second}}
}

func (p *ClientPool) SetConnectFunc(fn func(context.Context, config.MCPServerEntry) (upstreamClient, error)) {
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
		// connect (via p.connect) performs the full spawn/dial-and-handshake
		// in one call -- the official SDK's Client.Connect does the
		// initialize round trip internally, unlike mark3labs' separate
		// NewXxxClient + explicit Initialize -- bounded by its own internal
		// handshakeTimeout window. The notification handler for
		// tools/list_changed is wired inside p.connect too, since
		// ClientOptions.ToolListChangedHandler must be supplied before
		// Connect rather than registered afterward.
		client, err := p.connect(ctx, entry)
		if err == nil {
			p.mu.Lock()
			s.client = client
			if leaf, ok := client.(*stdioUpstream); ok {
				launch := leaf.launch
				s.lastLaunch = &launch
			}
			p.mu.Unlock()
			listCtx, cancel := context.WithTimeout(ctx, p.policy.handshakeTimeout)
			var result *mcpsdk.ListToolsResult
			result, err = client.ListTools(listCtx, &mcpsdk.ListToolsParams{})
			if err != nil {
				err = fmt.Errorf("list tools: %w", err)
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
				stable := time.NewTimer(p.policy.StableFor)
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
		delay, ok := p.policy.Next(s.restarts)
		if entry.Transport != "stdio" || !ok {
			s.state = "failed"
			s.nextRetry = time.Time{}
			if entry.Transport == "stdio" {
				s.exhausted = true
				s.err = fmt.Errorf("restart limit reached (%d); correct the upstream and start a new proxy: %w", p.policy.Limit(), s.err)
			}
			p.mu.Unlock()
			slog.Error("mcp-proxy: upstream recovery stopped", "server", entry.ID, "restart_limit", p.policy.Limit())
			return
		}
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
func (p *ClientPool) publish(ctx context.Context, id string, client upstreamClient, tools []*mcpsdk.Tool, notify bool) (ToolRefreshResult, error) {
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

func (p *ClientPool) fail(id string, client upstreamClient, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.statuses[id]; s != nil && s.client == client && s.state != "reconnecting" && !s.exhausted {
		s.err = err
		s.state = "failed"
		slog.Warn("mcp-proxy: upstream unavailable", "server", id, "err", err)
	}
}

// connect creates (and connects) the appropriate MCP client for the entry.
// ctx is the long-lived supervise ctx: it is threaded into the
// tools/list_changed notification handler (fired well after this call
// returns, so it must survive past this call's own handshake deadline) and
// used, bounded by its own internal handshakeTimeout window, for the
// connect+initialize handshake itself.
func (p *ClientPool) connect(ctx context.Context, entry config.MCPServerEntry) (upstreamClient, error) {
	p.mu.Lock()
	connectFn := p.connectFn
	p.mu.Unlock()
	if connectFn != nil {
		return connectFn(ctx, entry)
	}

	impl := &mcpsdk.Implementation{Name: "agent-mux-proxy", Version: p.runtime.Build.Version}
	opts := &mcpsdk.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) {
			go func() { _, _ = p.RefreshServer(ctx, entry.ID) }()
		},
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, p.policy.handshakeTimeout)
	defer cancel()

	switch entry.Transport {
	case "stdio":
		u, upTransport, err := spawnStdioUpstream(entry)
		if err != nil {
			return nil, err
		}
		// The RelaunchObservation is Tether announcing its own launch/recovery
		// context TO the upstream during the handshake -- it needs u.launch,
		// only known now that the process has actually spawned, so it is
		// built here rather than passed into spawnStdioUpstream.
		p.mu.Lock()
		observation := RelaunchObservation{SchemaVersion: 1, Mode: "observation-only", Owner: p.runtime, Launch: u.launch, Recovery: p.recoveryObservation(p.statuses[entry.ID])}
		p.mu.Unlock()
		opts.Capabilities = &mcpsdk.ClientCapabilities{Experimental: map[string]any{RuntimeObservationCapability: observation}}

		cs, err := mcpsdk.NewClient(impl, opts).Connect(handshakeCtx, upTransport, nil)
		if err != nil {
			u.abandon()
			return nil, fmt.Errorf("connect mcp stdio upstream %q: %w", entry.Command, err)
		}
		u.ClientSession = cs
		u.watchExit()
		return u, nil
	case "sse", "http":
		// go-mcp/client (v0.5.0+) owns dial/reconnect/health-probe for these
		// two transports -- see the remoteClients field doc. It has no
		// ToolListChangedHandler equivalent (dialSDK always passes nil
		// ClientOptions), so unlike stdio, a notification-driven refresh from
		// an sse/http upstream is not wired here; mux_catalog_refresh and the
		// periodic paths remain the way those two transports pick up a
		// changed tool list. Worth a go-mcp follow-up if a remote upstream
		// that relies on the notification shows up.
		if entry.URL == "" {
			return nil, fmt.Errorf("%s transport requires url", entry.Transport)
		}
		pool := p.remoteClientPool()
		cfg := gomcpclient.ServerConfig{Transport: entry.Transport, URL: entry.URL}
		if entry.Token != "" {
			cfg.Headers = map[string]string{"Authorization": "Bearer " + entry.Token}
		}
		if err := pool.Register(entry.ID, cfg); err != nil {
			return nil, fmt.Errorf("register %s upstream %q: %w", entry.Transport, entry.ID, err)
		}
		gc, err := pool.Get(entry.ID)
		if err != nil {
			return nil, err
		}
		// Ping forces the dial+handshake now, synchronously, matching every
		// other case's contract: connect() returning nil error means the
		// handshake already succeeded, not merely that a config was
		// recorded for a later lazy dial.
		if err := gc.Ping(handshakeCtx); err != nil {
			return nil, fmt.Errorf("start %s transport: %w", entry.Transport, err)
		}
		return &remoteClient{gc: gc}, nil
	default:
		return nil, fmt.Errorf("unknown transport %q (want stdio, sse, or http)", entry.Transport)
	}
}

// remoteClientPool lazily constructs p.remoteClients on first use, so its
// identity picks up p.runtime.Build.Version as RunWithProxyOpts sets it
// (after NewClientPool has already returned).
func (p *ClientPool) remoteClientPool() *gomcpclient.Pool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remoteClients == nil {
		p.remoteClients = gomcpclient.NewPool(gomcpclient.WithIdentity("agent-mux-proxy", p.runtime.Build.Version))
	}
	return p.remoteClients
}

// remoteClient adapts a go-mcp/client.Client (dial-on-first-use, retry-once,
// lazy health-probed reconnect for sse/http) to this package's upstreamClient
// interface.
//
// CallTool goes through the raw SDK session (Client.SDKSession) rather than
// go-mcp/client's own CallTool wrapper: that wrapper has no way to set
// _meta, and proxy.go's provenance/trace-context injection depends on
// setting Meta on every forwarded call. Ping first so the underlying
// Client's own dial-if-needed and lazy-probe-triggered reconnect run before
// SDKSession is read -- SDKSession returns nil when no connection is open.
type remoteClient struct {
	gc *gomcpclient.Client
}

func (r *remoteClient) CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	if err := r.gc.Ping(ctx); err != nil {
		return nil, err
	}
	sess := r.gc.SDKSession()
	if sess == nil {
		return nil, fmt.Errorf("go-mcp/client: no session open after a successful ping")
	}
	return sess.CallTool(ctx, params)
}

func (r *remoteClient) ListTools(ctx context.Context, _ *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
	return r.gc.ListTools(ctx)
}

func (r *remoteClient) Close() error {
	return r.gc.Close()
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
	ID                string              `json:"id"`
	Transport         string              `json:"transport"`
	Status            string              `json:"status"`
	Error             string              `json:"error,omitempty"`
	ToolCount         int                 `json:"tool_count"`
	Tags              []string            `json:"tags,omitempty"`
	RestartAttempts   int                 `json:"restart_attempts"`
	RestartLimit      int                 `json:"restart_limit"`
	NextRetryAt       *time.Time          `json:"next_retry_at,omitempty"`
	LastExit          *supervise.Exit     `json:"last_exit,omitempty"`
	StderrTail        string              `json:"stderr_tail,omitempty"`
	RecoveryExhausted bool                `json:"recovery_exhausted"`
	LastLaunch        *LaunchObservation  `json:"last_launch,omitempty"`
	Recovery          RecoveryObservation `json:"recovery"`
}

func (p *ClientPool) StatusSummary() []ServerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ServerStatus, 0, len(p.statuses))
	for _, s := range p.statuses {
		ss := ServerStatus{ID: s.entry.ID, Transport: s.entry.Transport, Tags: s.entry.Tags, ToolCount: s.toolCount, Status: s.state, RestartAttempts: s.restarts, LastExit: s.lastExit, StderrTail: s.stderr}
		ss.RecoveryExhausted = s.exhausted
		ss.Recovery = p.recoveryObservation(s)
		if s.lastLaunch != nil {
			launch := *s.lastLaunch
			ss.LastLaunch = &launch
		}
		if s.entry.Transport == "stdio" {
			ss.RestartLimit = p.policy.Limit()
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

func (p *ClientPool) refreshServer(ctx context.Context, id string, client upstreamClient, source string) (ToolRefreshResult, error) {
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
	result, err := client.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		callerErr := callerCtx.Err()
		if callerErr == nil || !errors.Is(err, callerErr) {
			p.fail(id, client, fmt.Errorf("refresh list tools (%s): %w", source, err))
		}
		return ToolRefreshResult{}, err
	}
	return p.publish(ctx, id, client, result.Tools, true)
}
