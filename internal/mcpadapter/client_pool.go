package mcpadapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	gomcpclient "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/client"
	"github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/supervise"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
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
	accepted   bool
	degraded   bool
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
	// kick wakes a remote upstream's supervisor (CW-20261114-0182): to retry
	// before nextRetry, or, from fail, because a connected upstream failed.
	// attempts counts finished connect attempts.
	kick     chan struct{}
	attempts int
	// explicit marks a pending kick as a refresh/reload (RetryNow), which
	// reconnects at once; any other wake goes through the backoff.
	explicit bool
	// waitStart is when the current backoff step began; earlyUsed says an
	// implicit Retry already brought this step's attempt forward.
	waitStart time.Time
	earlyUsed bool
}

// UpstreamStateChange is one upstream state transition, for the daemon's
// mcp.upstream_state_changed event (CW-20261114-0182).
type UpstreamStateChange struct {
	ServerID    string     `json:"server_id"`
	From        string     `json:"from"`
	To          string     `json:"to"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	Attempt     int        `json:"attempt,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// Remote (http/sse) upstream retry: capped exponential backoff with jitter,
// never giving up (CW-20261114-0182). Between attempts the upstream is
// "recovering", with next_retry_at; an attempt is "starting". Before, an http upstream that was down
// or restarting at its first handshake stayed "failed" until a hub reload.
var (
	remoteRetryBase = 5 * time.Second
	remoteRetryMax  = 5 * time.Minute
	// remoteRetryJitter spreads retries by up to this fraction either way.
	remoteRetryJitter = 0.2
)

// remoteRetryDelay is the wait before remote attempt n+1 (n from 0).
func remoteRetryDelay(n int) time.Duration {
	d := remoteRetryBase
	for i := 0; i < n && d < remoteRetryMax; i++ {
		d *= 2
	}
	d = min(d, remoteRetryMax)
	jitter := time.Duration((rand.Float64()*2 - 1) * remoteRetryJitter * float64(d)) //nolint:gosec // G404: jitter, not security
	return max(d+jitter, time.Millisecond)
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
	lazyMu             sync.Mutex
	lazyCtx            context.Context
	lazyClosed         bool
	lazyReady          map[string]chan struct{}
	confineRemote      bool
	probe              bool
	protectedPaths     []string // daemon pools wrap every stdio child; legacy proxies inherit their wrapper
	requireConfinement bool
	runtime            RuntimeObservation
	entries            []config.MCPServerEntry
	registry           *ToolRegistry
	mu                 sync.Mutex
	catalogMu          sync.Mutex // serialize registry changes and their publication
	statuses           map[string]*clientStatus
	refreshing         map[upstreamClient]bool
	connectFn          func(context.Context, config.MCPServerEntry) (upstreamClient, error)
	toolRefreshHandler func(ToolRefreshResult)
	stateHandler       func(UpstreamStateChange)
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
	// Set only by the daemon's upstream owner before Start. Per-entry pools
	// bind the SDK's entry-blind HTTP builder to explicit catalog policy.
	remoteHTTPClientFactory func(config.MCPServerEntry) (func(map[string]string, int) *http.Client, error)
	remoteClientsByEntry    map[string]*gomcpclient.Pool
}

// recoveryPolicy wraps go-mcp/supervise's Policy (backoff schedule +
// stable-for reset window) with handshakeTimeout, a Tether-local knob the
// shared package has no opinion on.
type recoveryPolicy struct {
	supervise.Policy
	handshakeTimeout time.Duration
}

func NewClientPool(entries []config.MCPServerEntry, registry *ToolRegistry) *ClientPool {
	for _, entry := range entries {
		if entry.ID != "" {
			registry.SetPrefix(entry.ID, entry.ToolPrefix)
		}
	}
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

// SetStateHandler installs fn to be told of every upstream state transition.
// It is called without the pool's lock held.
func (p *ClientPool) SetStateHandler(fn func(UpstreamStateChange)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stateHandler = fn
}

// setState moves s to state (caller holds p.mu) and returns the change to
// report once the lock is released, or nil when the state did not change.
func (p *ClientPool) setState(s *clientStatus, state string) func() {
	if s.state == state {
		return func() {}
	}
	return p.reportState(s, state)
}

// reportState is setState that reports even when the state is unchanged: the
// stdio restart limit turns a failed upstream into a terminally failed one.
func (p *ClientPool) reportState(s *clientStatus, state string) func() {
	change := UpstreamStateChange{ServerID: s.entry.ID, From: s.state, To: state, Attempt: s.restarts}
	s.state = state
	if !s.nextRetry.IsZero() && (state == "reconnecting" || state == "recovering") {
		next := s.nextRetry
		change.NextRetryAt = &next
	}
	if s.err != nil && state != "connected" {
		change.Error = p.redactError(s.err, s.entry).Error()
	}
	handler := p.stateHandler
	return func() {
		if handler != nil {
			handler(change)
		}
	}
}

// Retry is the implicit retry of a tool list, search or call that needs a
// down remote upstream. It respects the backoff: it can bring a step's attempt
// forward at most once, and not before half of that step's wait has passed,
// so a dead upstream is never redialed back to back however often it is asked
// for (#581 review). It reports whether an attempt was requested.
func (p *ClientPool) Retry(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.statuses[id]
	if s == nil || s.kick == nil || s.entry.Transport == "stdio" || s.state != "recovering" || s.earlyUsed || s.nextRetry.IsZero() {
		return false
	}
	if time.Now().UTC().Before(s.waitStart.Add(s.nextRetry.Sub(s.waitStart) / 2)) {
		return false
	}
	s.earlyUsed = true
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return true
}

// RetryNow is the explicit retry of a refresh or reload: unconditional, it
// reconnects a remote upstream that is not connected at once.
func (p *ClientPool) RetryNow(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.statuses[id]
	if s == nil || s.kick == nil || s.entry.Transport == "stdio" || s.state == "connected" || s.state == "excluded" {
		return false
	}
	s.explicit = true
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return true
}

// RetryUnavailable is Retry for each id in ids.
func (p *ClientPool) RetryUnavailable(ids map[string]bool) {
	for id := range ids {
		p.Retry(id)
	}
}

// Start waits for the first bounded handshake per server, then leaves each
// supervisor running until Shutdown. Failed siblings do not abort startup.
func (p *ClientPool) Start(ctx context.Context) error {
	ids := []string{}
	for _, entry := range p.entries {
		if entry.IsEnabled() {
			ids = append(ids, entry.ID)
		}
	}
	originErr := mcpgateway.ValidateOriginIDs(ids)
	ctx, p.cancel = context.WithCancel(ctx)
	var initial sync.WaitGroup
	seen := map[string]bool{}
	for _, entry := range p.entries {
		if !entry.IsEnabled() {
			continue
		}
		p.mu.Lock()
		p.statuses[entry.ID] = &clientStatus{entry: entry, state: "starting", kick: make(chan struct{}, 1)}
		invalid := entry.ID == "tether" || entry.ID == "" || seen[entry.ID]
		seen[entry.ID] = true
		if invalid {
			p.statuses[entry.ID].state = "excluded"
			p.statuses[entry.ID].err = originErr
		}
		p.mu.Unlock()
		if invalid {
			continue
		}
		if err := confineUpstreamTransport(entry, p.confineRemote); err != nil {
			p.mu.Lock()
			p.statuses[entry.ID].state = "excluded"
			p.statuses[entry.ID].err = err
			p.mu.Unlock()
			slog.Warn("mcp-proxy: upstream excluded", "server", entry.ID, "reason", err.Error())
			continue
		}
		if p.confineRemote && entry.AllowUnconfinedRemote && entry.Transport != "stdio" {
			slog.Warn("mcp-proxy: operator opted in to an unconfined remote upstream", "server", entry.ID)
		}
		initial.Add(1)
		p.workers.Add(1)
		go func() { defer p.workers.Done(); p.supervise(ctx, entry, initial.Done) }()
	}
	initial.Wait()
	collisions := p.registry.Collisions()
	if len(collisions) > 0 {
		p.mu.Lock()
		for _, collision := range collisions {
			for _, owner := range collision.Owners {
				if status := p.statuses[owner.Origin]; status != nil {
					status.degraded = true
					status.err = &mcpgateway.CollisionError{Collisions: []mcpgateway.NameCollision{collision}}
				}
			}
		}
		p.mu.Unlock()
		return errors.Join(originErr, &mcpgateway.CollisionError{Collisions: collisions})
	}
	return originErr
}

func (p *ClientPool) supervise(ctx context.Context, entry config.MCPServerEntry, ready func()) {
	var readyOnce sync.Once
	first := true
	defer readyOnce.Do(ready)
	for ctx.Err() == nil {
		p.mu.Lock()
		s := p.statuses[entry.ID]
		s.nextRetry = time.Time{}
		s.client = nil
		report := p.setState(s, "starting")
		p.mu.Unlock()
		report()
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
			if initialized, ok := client.(interface {
				InitializeResult() *mcpsdk.InitializeResult
			}); ok {
				if result := initialized.InitializeResult(); result != nil {
					p.registry.RecordInstructions(entry.ID, utf8.RuneCountInString(result.Instructions))
				}
			}
			p.mu.Lock()
			s.client = client
			if leaf, ok := client.(*stdioUpstream); ok {
				launch := leaf.launch
				s.lastLaunch = &launch
			}
			p.mu.Unlock()
			listCtx, cancel := context.WithTimeout(ctx, p.policy.handshakeTimeout)
			var result *mcpsdk.ListToolsResult
			result, err = listUpstreamTools(listCtx, client)
			if err != nil {
				err = fmt.Errorf("list tools: %w", err)
			}
			cancel()
			if err == nil {
				_, err = p.publish(ctx, entry.ID, client, result.Tools, !first)
			}
		}
		if err != nil && !first {
			var collision *mcpgateway.CollisionError
			if errors.As(err, &collision) {
				p.catalogMu.Lock()
				p.mu.Lock()
				handler := p.toolRefreshHandler
				var rebound *ToolRefreshResult
				report := func() {}
				if s.accepted {
					p.registry.RebindServer(entry.ID, client)
					s.degraded = true
					s.err = err
					report = p.setState(s, "connected")
					err = nil
					rebound = &ToolRefreshResult{ServerID: entry.ID, ToolCount: s.toolCount}
				}
				p.mu.Unlock()
				report()
				if rebound != nil && handler != nil {
					handler(*rebound)
				}
				p.catalogMu.Unlock()
			}
		}
		if err != nil {
			p.fail(entry.ID, client, err)
			if client != nil {
				_ = client.Close()
			}
		}
		p.mu.Lock()
		s.attempts++
		if err == nil && entry.Transport != "stdio" {
			s.restarts = 0
		}
		p.mu.Unlock()
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
			} else if err == nil {
				// HTTP/SSE have no child to respawn; the client pool redials a
				// dropped connection on use. Stay until shutdown, or until the
				// upstream fails mid-life (fail wakes us): then reconnect, at once
				// for a refresh/reload, else through the backoff below
				// (CW-20261114-0182).
				reconnect, immediate := false, false
				for !reconnect {
					select {
					case <-ctx.Done():
						_ = client.Close()
						return
					case <-s.kick:
						p.mu.Lock()
						reconnect = s.state != "connected"
						immediate, s.explicit = s.explicit, false
						p.mu.Unlock()
					}
				}
				_ = client.Close()
				if immediate {
					continue
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		if entry.Transport != "stdio" {
			// A remote upstream that is down or restarting is retried with
			// backoff, without limit, and at once when Retry asks.
			p.mu.Lock()
			delay := remoteRetryDelay(s.restarts)
			s.waitStart = time.Now().UTC()
			s.nextRetry = s.waitStart.Add(delay)
			s.earlyUsed = false
			report := p.setState(s, "recovering")
			p.mu.Unlock()
			report()
			slog.Warn("mcp-proxy: remote upstream retry scheduled", "server", entry.ID, "delay", delay) //nolint:gosec // G706 false positive: structured slog attrs are quoted/encoded by the handler; entry.ID is a catalog server id
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-s.kick:
				timer.Stop()
			}
			p.mu.Lock()
			s.restarts++
			s.explicit = false
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		delay, ok := p.policy.Next(s.restarts)
		if !ok {
			s.nextRetry = time.Time{}
			s.exhausted = true
			s.err = fmt.Errorf("restart limit reached (%d); correct the upstream and start a new proxy: %w", p.policy.Limit(), s.err)
			report := p.reportState(s, "failed")
			p.mu.Unlock()
			report()
			slog.Error("mcp-proxy: upstream recovery stopped", "server", entry.ID, "restart_limit", p.policy.Limit())
			return
		}
		s.nextRetry = time.Now().UTC().Add(delay)
		report = p.setState(s, "reconnecting")
		p.mu.Unlock()
		report()
		slog.Warn("mcp-proxy: upstream restart scheduled", "server", entry.ID, "delay", delay) //nolint:gosec // G706 false positive: structured slog attrs are quoted/encoded by the handler; entry.ID is a catalog server id
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
	delta, err := p.registry.ReplaceServer(id, client, tools)
	if err != nil {
		s.err = err
		s.degraded = true
		p.mu.Unlock()
		return ToolRefreshResult{}, err
	}
	s.toolCount = len(tools)
	s.accepted = true
	s.degraded = false
	s.err = nil
	s.nextRetry = time.Time{}
	report := p.setState(s, "connected")
	handler := p.toolRefreshHandler
	p.mu.Unlock()
	report()
	refresh := ToolRefreshResult{ServerID: id, ToolCount: len(tools), Delta: delta}
	if notify && handler != nil {
		handler(refresh)
	}
	slog.Info("mcp-proxy: upstream tools ready", "server", id, "tools", len(tools))
	return refresh, nil
}

func (p *ClientPool) fail(id string, client upstreamClient, err error) {
	report := func() {}
	defer func() { report() }()
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.statuses[id]; s != nil && s.client == client && s.state != "reconnecting" && !s.exhausted {
		// Scrub before the error is stored (ServerStatus.Error) or logged: an
		// endpoint in the text may be a secret.
		err = p.redactError(err, s.entry)
		if p.probe {
			err = &redactedError{text: redactProbeText(probeRedactor([]config.MCPServerEntry{s.entry}), err.Error()), err: err}
		}
		s.err = err
		midLife := s.state == "connected" && s.entry.Transport != "stdio"
		report = p.setState(s, "failed")
		slog.Warn("mcp-proxy: upstream unavailable", "server", id, "err", err)
		if midLife && s.kick != nil {
			// A connected remote upstream failed: wake its supervisor into the
			// timed retry (#581 review).
			select {
			case s.kick <- struct{}{}:
			default:
			}
		}
	}
}

// connect creates (and connects) the appropriate MCP client for the entry.
// ctx is the long-lived supervise ctx: it is threaded into the
// tools/list_changed notification handler (fired well after this call
// returns, so it must survive past this call's own handshake deadline) and
// used, bounded by its own internal handshakeTimeout window, for the
// connect+initialize handshake itself.
func (p *ClientPool) connect(ctx context.Context, entry config.MCPServerEntry) (upstreamClient, error) {
	if err := confineUpstreamTransport(entry, p.confineRemote); err != nil {
		return nil, err
	}
	p.mu.Lock()
	connectFn := p.connectFn
	p.mu.Unlock()
	if connectFn != nil {
		return connectFn(ctx, entry)
	}

	impl := &mcpsdk.Implementation{Name: "tether-proxy", Version: p.runtime.Build.Version}
	opts := &mcpsdk.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) {
			go func() { _, _ = p.RefreshServer(ctx, entry.ID) }()
		},
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, p.policy.handshakeTimeout)
	defer cancel()

	switch entry.Transport {
	case "stdio":
		var lifetime []context.Context
		if p.probe {
			lifetime = []context.Context{ctx}
		}
		u, upTransport, err := spawnStdioUpstreamConfined(entry, p.protectedPaths, p.requireConfinement, lifetime...)
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
		// an sse/http upstream is not wired here; tether_catalog_refresh and the
		// periodic paths remain the way those two transports pick up a
		// changed tool list. Worth a go-mcp follow-up if a remote upstream
		// that relies on the notification shows up.
		if entry.URL == "" {
			return nil, fmt.Errorf("%s transport requires url", entry.Transport)
		}
		pool, err := p.remoteClientPoolForEntry(entry)
		if err != nil {
			return nil, err
		}
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
		remote := &remoteClient{gc: gc}
		if p.remoteHTTPClientFactory != nil && entry.ProxyServiceTokenFile != "" {
			remote.serviceToken = entry.Token
		}
		return remote, nil
	default:
		return nil, fmt.Errorf("unknown transport %q (want stdio, sse, or http)", entry.Transport)
	}
}

func (p *ClientPool) remoteClientPoolForEntry(entry config.MCPServerEntry) (*gomcpclient.Pool, error) {
	var build func(map[string]string, int) *http.Client
	if p.remoteHTTPClientFactory != nil {
		var err error
		build, err = p.remoteHTTPClientFactory(entry)
		if err != nil {
			return nil, err
		}
	}
	if build == nil && entry.Transport == "sse" {
		build = ordinarySSEHTTPClient
	}
	if build == nil {
		return p.remoteClientPool(), nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remoteClientsByEntry == nil {
		p.remoteClientsByEntry = make(map[string]*gomcpclient.Pool)
	}
	if pool := p.remoteClientsByEntry[entry.ID]; pool != nil {
		return pool, nil
	}
	pool := gomcpclient.NewPool(gomcpclient.WithIdentity("tether-proxy", p.runtime.Build.Version), gomcpclient.WithHTTPClient(build))
	p.remoteClientsByEntry[entry.ID] = pool
	return pool, nil
}

// remoteClientPool lazily constructs p.remoteClients on first use, so its
// identity picks up p.runtime.Build.Version as RunWithProxyOpts sets it
// (after NewClientPool has already returned).
func (p *ClientPool) remoteClientPool() *gomcpclient.Pool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remoteClients == nil {
		p.remoteClients = gomcpclient.NewPool(gomcpclient.WithIdentity("tether-proxy", p.runtime.Build.Version))
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
	gc           *gomcpclient.Client
	serviceToken string
}

func (r *remoteClient) CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	if err := r.gc.Ping(ctx); err != nil {
		return nil, err
	}
	sess := r.gc.SDKSession()
	if sess == nil {
		return nil, fmt.Errorf("go-mcp/client: no session open after a successful ping")
	}
	result, err := sess.CallTool(withForwardedToolCall(ctx), params)
	if err != nil || r.serviceToken == "" || result == nil {
		return result, err
	}
	return scrubServiceToolResult(result, r.serviceToken)
}

func (r *remoteClient) ListTools(ctx context.Context, _ *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
	return r.gc.ListTools(ctx)
}

func (r *remoteClient) InitializeResult() *mcpsdk.InitializeResult {
	if session := r.gc.SDKSession(); session != nil {
		return session.InitializeResult()
	}
	return nil
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
	client, state, remote := s.client, s.state, s.entry.Transport != "stdio"
	p.mu.Unlock()
	if remote && (client == nil || state != "connected") && state != "excluded" {
		// A refresh re-probes a remote upstream that is down rather than
		// reporting the stale failure (CW-20261114-0182).
		return p.retryRemote(ctx, id)
	}
	if client == nil || state == "starting" || state == "reconnecting" {
		return ToolRefreshResult{}, fmt.Errorf("upstream %q unavailable (%s)", id, state)
	}
	return p.refreshServer(ctx, id, client, "manual")
}

// retryRemote asks a remote upstream's supervisor for an attempt now and waits
// (bounded by the handshake timeout and ctx) for it to finish.
func (p *ClientPool) retryRemote(ctx context.Context, id string) (ToolRefreshResult, error) {
	p.mu.Lock()
	before := p.statuses[id].attempts
	p.mu.Unlock()
	if !p.RetryNow(id) {
		return ToolRefreshResult{}, fmt.Errorf("upstream %q cannot be retried", id)
	}
	deadline := time.NewTimer(p.policy.handshakeTimeout + time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		p.mu.Lock()
		s := p.statuses[id]
		done, state, count, lastErr := s.attempts > before, s.state, s.toolCount, s.err
		p.mu.Unlock()
		if done && state == "connected" {
			return ToolRefreshResult{ServerID: id, ToolCount: count}, nil
		}
		if done {
			if lastErr != nil {
				return ToolRefreshResult{}, fmt.Errorf("upstream %q unavailable (%s): %w", id, state, p.redactError(lastErr, s.entry))
			}
			return ToolRefreshResult{}, fmt.Errorf("upstream %q unavailable (%s)", id, state)
		}
		select {
		case <-ctx.Done():
			return ToolRefreshResult{}, ctx.Err()
		case <-deadline.C:
			return ToolRefreshResult{}, fmt.Errorf("upstream %q unavailable (%s): retry still running", id, state)
		case <-tick.C:
		}
	}
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
	p.lazyMu.Lock()
	p.lazyClosed = true
	p.lazyMu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	p.workers.Wait()
}

// ServerStatus reports observed connection state, not an active health probe.
// ToolCount is currently available inventory. CatalogedTools retains accepted
// cached definitions; InventoryExamined distinguishes no observation from zero.
type ServerStatus struct {
	ID                string              `json:"id"`
	Degraded          bool                `json:"degraded"`
	Transport         string              `json:"transport"`
	Status            string              `json:"status"`
	Error             string              `json:"error,omitempty"`
	ToolCount         int                 `json:"tool_count"`
	CatalogedTools    int                 `json:"cataloged_tools"`
	InventoryExamined bool                `json:"inventory_examined"`
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
	collisions := p.registry.Collisions()
	affected := map[string][]mcpgateway.NameCollision{}
	for _, collision := range collisions {
		for _, owner := range collision.Owners {
			affected[owner.Origin] = append(affected[owner.Origin], collision)
		}
	}
	out := make([]ServerStatus, 0, len(p.statuses))
	for _, s := range p.statuses {
		if findings := affected[s.entry.ID]; len(findings) > 0 {
			s.degraded = true
			if s.accepted {
				s.err = &mcpgateway.CollisionError{Collisions: findings}
			}
		} else {
			var collision *mcpgateway.CollisionError
			if s.accepted && errors.As(s.err, &collision) {
				s.degraded = false
				s.err = nil
			}
		}
		ss := ServerStatus{Degraded: s.degraded, ID: s.entry.ID, Transport: s.entry.Transport, Tags: s.entry.Tags, CatalogedTools: s.toolCount, InventoryExamined: s.accepted, Status: s.state, RestartAttempts: s.restarts, LastExit: s.lastExit, StderrTail: s.stderr}
		if s.state == "connected" {
			ss.ToolCount = s.toolCount
		}
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
			ss.Error = p.redactError(s.err, s.entry).Error()
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
	var entry config.MCPServerEntry
	if s := p.statuses[id]; s != nil {
		entry = s.entry
	}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.refreshing, client); p.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, p.policy.handshakeTimeout)
	defer cancel()
	result, err := listUpstreamTools(ctx, client)
	if err != nil {
		callerErr := callerCtx.Err()
		if callerErr == nil || !errors.Is(err, callerErr) {
			p.fail(id, client, fmt.Errorf("refresh list tools (%s): %w", source, err))
		}
		// The caller (tether_catalog_refresh, a sysop probe) shows this text.
		return ToolRefreshResult{}, p.redactError(err, entry)
	}
	return p.publish(ctx, id, client, result.Tools, true)
}

// Read every tools/list page under one deadline before validating final names.
func listUpstreamTools(ctx context.Context, client upstreamClient) (*mcpsdk.ListToolsResult, error) {
	out := &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{}}
	params := &mcpsdk.ListToolsParams{}
	seen := map[string]bool{}
	for {
		result, err := client.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		if result == nil {
			return nil, fmt.Errorf("upstream returned nil tools/list result")
		}
		out.Tools = append(out.Tools, result.Tools...)
		if result.NextCursor == "" {
			return out, nil
		}
		if seen[result.NextCursor] {
			return nil, fmt.Errorf("upstream repeated tools/list cursor")
		}
		seen[result.NextCursor] = true
		params.Cursor = result.NextCursor
	}
}
