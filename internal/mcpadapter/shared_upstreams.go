package mcpadapter

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SharedUpstreams is the daemon's single upstream owner. Views only restrict
// inventory/dispatch; they never dial, spawn, or close an upstream connection.
// The transport composition root owns Start/Close, not individual clients.
type SharedUpstreams struct {
	pool        *ClientPool
	registry    *ToolRegistry
	known       map[string]bool
	tags        map[string][]string
	external    map[string]bool
	servedAt    map[string]string
	lifecycle   sync.Mutex
	started     bool
	closed      bool
	mu          sync.Mutex
	next        uint64
	subscribers map[uint64]func(ToolRefreshResult)
}

func NewSharedUpstreams(entries []config.MCPServerEntry, roots DaemonProtectedRoots, requireConfinement bool) (*SharedUpstreams, error) {
	if !requireConfinement {
		return nil, fmt.Errorf("daemon MCP upstream confinement is required")
	}
	protected, err := roots.paths()
	if err != nil {
		return nil, err
	}
	r := &SharedUpstreams{registry: NewToolRegistry(), known: map[string]bool{}, external: map[string]bool{}, servedAt: map[string]string{}, tags: map[string][]string{}, subscribers: map[uint64]func(ToolRefreshResult){}}
	for _, entry := range entries {
		if _, exists := r.known[entry.ID]; exists || entry.ID == "" || entry.ID == "tether" {
			return nil, fmt.Errorf("duplicate, empty or reserved daemon MCP upstream ID")
		}
		r.known[entry.ID] = entry.IsEnabled()
		r.external[entry.ID] = entry.IsExternal()
		r.servedAt[entry.ID] = entry.ServedAt
		r.tags[entry.ID] = append([]string(nil), entry.Tags...)
	}
	local := []config.MCPServerEntry{}
	for _, entry := range entries {
		if !entry.IsExternal() {
			local = append(local, entry)
		}
	}
	private, factory, err := daemonHTTPPolicies(local)
	if err != nil {
		return nil, err
	}
	r.pool = NewClientPool(private, r.registry)
	r.pool.remoteHTTPClientFactory = factory
	r.pool.confineRemote = true
	r.pool.protectedPaths = protected
	r.pool.requireConfinement = true
	r.pool.SetToolRefreshHandler(r.publish)
	return r, nil
}

// SetStateHandler installs fn to be told of every upstream state transition
// (connected, failed, reconnecting with its next attempt). Set it before Start.
func (r *SharedUpstreams) SetStateHandler(fn func(UpstreamStateChange)) {
	r.pool.SetStateHandler(fn)
}

// Start is idempotent: N clients cannot accidentally start N copies.
func (r *SharedUpstreams) Start(ctx context.Context) error {
	ids := []string{}
	for id, enabled := range r.known {
		if enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return r.StartOrigins(ctx, ids)
}

// StartOrigins starts only the admitted view's origins; supervisors are shared.
func (r *SharedUpstreams) StartOrigins(ctx context.Context, ids []string) error {
	r.lifecycle.Lock()
	if r.closed {
		r.lifecycle.Unlock()
		return fmt.Errorf("daemon MCP upstream runtime closed")
	}
	r.started = true
	r.lifecycle.Unlock()
	return r.pool.startOrigins(ctx, ids)
}

func (r *SharedUpstreams) Close() {
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.started {
		r.pool.Shutdown()
	}
}

// Subscribe fans out accepted registry updates. Callbacks must not call a pool
// refresh recursively; pool publication serializes generations. Unsubscribe
// prevents later publications but a callback already copied may finish.
func (r *SharedUpstreams) Subscribe(fn func(ToolRefreshResult)) func() {
	r.mu.Lock()
	r.next++
	id := r.next
	r.subscribers[id] = fn
	r.mu.Unlock()
	return func() { r.mu.Lock(); delete(r.subscribers, id); r.mu.Unlock() }
}

func (r *SharedUpstreams) publish(refresh ToolRefreshResult) {
	r.mu.Lock()
	callbacks := make([]func(ToolRefreshResult), 0, len(r.subscribers))
	for _, fn := range r.subscribers {
		callbacks = append(callbacks, fn)
	}
	r.mu.Unlock()
	for _, fn := range callbacks {
		fn(refresh)
	}
}

// UpstreamView has explicit grants: nil and [] both mean zero. The caller's
// verified principal resolver supplies servers, never an HTTP client flag.
type UpstreamView struct {
	runtime     *SharedUpstreams
	liveServers func() ([]string, error)
	servers     map[string]bool
	isExternal  func(string) bool
	Service     *mcpgateway.Service
}

func (r *SharedUpstreams) OpenView(owner string, servers []string, selection mcpgateway.Selection) (*UpstreamView, error) {
	if err := (&config.Catalog{MCPServerEnabled: r.known}).ValidateMCPGrant(owner, servers); err != nil {
		return nil, err
	}
	if err := mcpgateway.ValidateMode(string(selection.Mode)); err != nil {
		return nil, err
	}
	v := &UpstreamView{runtime: r, servers: map[string]bool{}}
	for _, id := range servers {
		v.servers[id] = true
	}
	router := NewProxyRouter(r.registry)
	router.pool = r.pool
	v.Service = &mcpgateway.Service{Selection: selection, Snapshot: v.snapshot,
		Dispatch: func(ctx context.Context, name string, args, meta map[string]any) (*mcpsdk.CallToolResult, error) {
			return router.Handle(ctx, ToolCall{ToolName: name, Args: args, Meta: meta})
		}}
	return v, nil
}

// includes rechecks ownership because an existing view survives catalog reloads.
func (v *UpstreamView) includes(id string) bool {
	if !v.servers[id] {
		return false
	}
	if v.isExternal != nil {
		return !v.isExternal(id)
	}
	return !v.runtime.external[id]
}

func (v *UpstreamView) snapshot() mcpgateway.Snapshot {
	s := mcpgateway.Snapshot{Entries: []mcpgateway.Entry{}, Origins: []mcpgateway.OriginStatus{}}
	s.Lint, s.Collisions, _ = v.namingDiagnostics()
	for _, status := range v.StatusSummary() {
		if v.includes(status.ID) {
			s.Origins = append(s.Origins, mcpgateway.OriginStatus{ID: status.ID, Degraded: status.Degraded, Status: status.Status, ToolCount: status.CatalogedTools, InventoryExamined: status.InventoryExamined, Error: status.Error})
			if status.Status != "connected" {
				// A tool list or call that would use a down remote upstream
				// retries it now (CW-20261114-0182).
				v.runtime.pool.Retry(status.ID)
			}
		}
	}
	for _, def := range v.runtime.registry.AllDefinitions() {
		tool, found := v.runtime.registry.Lookup(def.Name)
		if found && v.includes(tool.ServerID) {
			s.Entries = append(s.Entries, mcpgateway.Entry{Tool: def, Origin: tool.ServerID, Tags: v.runtime.tags[tool.ServerID]})
		}
	}
	sort.Slice(s.Origins, func(i, j int) bool { return s.Origins[i].ID < s.Origins[j].ID })
	return s
}

// StatusSummary is the only upstream status source installed on a native
// daemon adapter. Hidden origins never reach health, gateway status or errors.
func (v *UpstreamView) StatusSummary() []ServerStatus {
	statuses := []ServerStatus{}
	_, _, private := v.namingDiagnostics()
	for _, status := range v.runtime.pool.StatusSummary() {
		if v.includes(status.ID) {
			if private[status.ID] {
				status.Error = "tool name collision; other owner outside this view"
			}
			statuses = append(statuses, status)
		}
	}
	return statuses
}

func (v *UpstreamView) namingDiagnostics() ([]mcpgateway.NameFinding, []mcpgateway.NameCollision, map[string]bool) {
	allLint, allCollisions := v.runtime.registry.NameDiagnostics()
	lint := []mcpgateway.NameFinding{}
	collisions := []mcpgateway.NameCollision{}
	private := map[string]bool{}
	for _, finding := range allLint {
		if v.includes(finding.Origin) {
			lint = append(lint, finding)
		}
	}
	for _, collision := range allCollisions {
		visible := true
		for _, owner := range collision.Owners {
			if owner.Origin != "tether" && !v.includes(owner.Origin) {
				visible = false
			}
		}
		if visible {
			collisions = append(collisions, collision)
		} else {
			for _, owner := range collision.Owners {
				if v.includes(owner.Origin) {
					private[owner.Origin] = true
				}
			}
		}
	}
	return lint, collisions, private
}

// Refresh cannot select or expose an origin outside this view's grants.
func (v *UpstreamView) Refresh(ctx context.Context, server string) ([]ToolRefreshResult, error) {
	allowed := v.servers
	if v.liveServers != nil {
		ids, err := v.liveServers()
		if err != nil {
			return nil, err
		}
		allowed = map[string]bool{}
		for _, id := range ids {
			if v.servers[id] {
				allowed[id] = true
			}
		}
	}
	ids := []string{}
	if server != "" {
		if !allowed[server] || !v.includes(server) {
			return nil, fmt.Errorf("unknown or excluded MCP origin")
		}
		ids = append(ids, server)
	} else {
		for id := range allowed {
			if !v.includes(id) {
				continue
			}
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	results := []ToolRefreshResult{}
	failures := map[string]error{}
	for _, id := range ids {
		result, err := v.runtime.pool.RefreshServer(ctx, id)
		if err != nil {
			failures[id] = err
		} else {
			results = append(results, result)
		}
	}
	if len(failures) != 0 {
		return results, &RefreshAllError{Failures: failures}
	}
	return results, nil
}
