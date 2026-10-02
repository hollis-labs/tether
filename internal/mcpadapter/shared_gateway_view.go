package mcpadapter

import (
	"context"
	"fmt"
	"sync"

	gomcp "github.com/hollis-labs/go-mcp/server"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// GatewayView owns only per-caller SDK/native dispatch state. Close leaves the
// shared upstream pool running for other callers and disconnect/reconnect.
type GatewayView struct {
	Server  *mcpsdk.Server
	Service *mcpgateway.Service
	close   func()
}

func (v *GatewayView) Close() { v.close() }

// NewGatewayView builds a transport-neutral server. ServerFilter is the
// credential's resolved grant (nil is ZERO here); profile/mode selectors are
// presentation restrictions on that grant. The adapter must be verified.
func (r *SharedUpstreams) NewGatewayView(ctx context.Context, a *Adapter, opts ProxyOptions) (*GatewayView, error) {
	if a == nil || a.principal == nil {
		return nil, fmt.Errorf("verified daemon MCP adapter required")
	}
	if opts.Profile.Profile != nil {
		if err := opts.Profile.Profile.Validate(); err != nil {
			return nil, err
		}
		opts.ModeInputs.Profile = opts.Profile.Profile
	}
	selection, err := mcpgateway.ResolveMode(opts.ModeInputs)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{"tether": true}
	for id, enabled := range r.known {
		known[id] = enabled
	}
	grant := append([]string{}, opts.ServerFilter...)
	for _, floor := range opts.AuthorityProfiles {
		if floor.Profile == nil {
			return nil, mcpgateway.ErrInvalidSessionMCPPolicy
		}
		if err := floor.Profile.Validate(); err != nil {
			return nil, err
		}
		grant, err = mcpgateway.SelectOrigins(known, grant, floor.Profile)
		if err != nil {
			return nil, err
		}
	}
	selected, err := mcpgateway.SelectOrigins(known, grant, opts.Profile.Profile)
	if err != nil {
		return nil, err
	}
	upstreams, err := r.OpenView("verified principal "+a.principal.ID, selected, selection)
	if err != nil {
		return nil, err
	}
	a.upstreams = upstreams
	registry := NewToolRegistry()
	for _, entry := range r.pool.entries {
		if upstreams.servers[entry.ID] {
			registry.SetPrefix(entry.ID, entry.ToolPrefix)
		}
	}
	router := NewProxyRouter(registry)
	router.pool = r.pool
	router.SetLogger(a.logger())
	if a.readsViaDaemon() || (a.svc != nil && a.svc.Store != nil) {
		router.SetWorkstreamResolver(a.sessionWorkstreamID)
	}
	instructions := ""
	if opts.Profile.Profile != nil {
		instructions = opts.Profile.Profile.Instructions
	}
	s := a.newBareServer(gomcp.WithInstructions(instructions))
	var nativeClient *mcpsdk.ClientSession
	var nativeServer *mcpsdk.ServerSession
	var unsubscribe func()
	var closeOnce sync.Once
	var updateMu sync.Mutex
	closed := false
	closeView := func() {
		closeOnce.Do(func() {
			updateMu.Lock()
			closed = true
			updateMu.Unlock()
			if unsubscribe != nil {
				unsubscribe()
			}
			for session := range s.SDKServer().Sessions() {
				_ = session.Close()
			}
			if nativeClient != nil {
				_ = nativeClient.Close()
			}
			if nativeServer != nil {
				_ = nativeServer.Close()
			}
		})
	}
	success := false
	defer func() {
		if !success {
			closeView()
		}
	}()
	if !opts.Only {
		native := a.newServer()
		a.addTool(native, gomcp.Tool{Name: "tether_catalog_refresh", Description: "Refresh eligible upstream tool inventories in the shared daemon pool.", InputSchema: gomcp.InputSchema(gomcp.StringProp("server", "Optional eligible origin; omitted refreshes only this view's origins", false)), Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return upstreams.Refresh(ctx, str(args, "server"))
		}}, Writes())
		if opts.ProxyStore != nil {
			a.registerToolCallEventsTool(native, opts.ProxyStore)
		}
		st, ct := mcpsdk.NewInMemoryTransports()
		nativeServer, err = native.SDKServer().Connect(ctx, st, nil)
		if err != nil {
			return nil, err
		}
		nativeClient, err = mcpsdk.NewClient(&mcpsdk.Implementation{Name: "tether-daemon-native-dispatch", Version: "1"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			return nil, err
		}
		for tool, listErr := range nativeClient.Tools(ctx, nil) {
			if listErr != nil {
				return nil, listErr
			}
			if err := registry.RegisterLocal(tool, nativeClient); err != nil {
				return nil, err
			}
		}
	}
	syncOrigin := func(id string) error {
		if !upstreams.servers[id] {
			return nil
		}
		defs := []*mcpsdk.Tool{}
		var client upstreamClient
		for _, def := range r.registry.AllDefinitions() {
			tool, found := r.registry.Lookup(def.Name)
			if !found || tool.ServerID != id {
				continue
			}
			// Shared definitions already carry final names. Reconstruct the
			// original name before applying this view's same declared prefix,
			// preserving the dispatch identity and authored metadata.
			original := *def
			original.Name = tool.UpstreamName
			defs = append(defs, &original)
			client = tool.Client
		}
		_, err := registry.ReplaceServer(id, client, defs)
		return err
	}
	r.pool.catalogMu.Lock()
	defer r.pool.catalogMu.Unlock()
	for _, id := range selected {
		if err := syncOrigin(id); err != nil {
			return nil, err
		}
	}
	gateway := a.gatewayService(registry, router, selection, r.tags)
	fullSnapshot := gateway.Snapshot
	gateway.Snapshot = func() mcpgateway.Snapshot {
		full := fullSnapshot()
		_, collisions, _ := upstreams.namingDiagnostics()
		full.Collisions = append(full.Collisions, collisions...)
		origins := []mcpgateway.OriginStatus{}
		for _, origin := range full.Origins {
			if origin.ID == "tether" || upstreams.servers[origin.ID] {
				origins = append(origins, origin)
			}
		}
		full.Origins = origins
		return full
	}
	order := []string{"tether"}
	for _, entry := range r.pool.entries {
		if upstreams.servers[entry.ID] {
			order = append(order, entry.ID)
		}
	}
	if opts.Profile.Profile != nil && opts.Profile.Profile.Servers != nil {
		order = append([]string{}, opts.Profile.Profile.Servers...)
	}
	gateway.Policy = &mcpgateway.Policy{Selection: opts.Profile, Floors: opts.AuthorityProfiles, ServerOrder: order}
	if err := gateway.Policy.ValidateNames(gateway.Snapshot()); err != nil {
		return nil, err
	}
	s.SDKServer().AddReceivingMiddleware(gatewaySurfaceMiddleware(gateway))
	if !opts.Only {
		a.registerDocsResources(s)
		s.SDKServer().AddReceivingMiddleware(docsResourceMiddleware(gateway))
	}
	if opts.Publisher != nil {
		logging := NewLoggingMiddleware(opts.Publisher).RedactWith(proxyRedactionSet(r.pool.entries))
		logging.profile, logging.mode = opts.Profile.ID, string(selection.Mode)
		logging.contextDecorator = a.withSessionID
		s.SDKServer().AddReceivingMiddleware(proxyLoggingMiddleware([]ToolCallMiddleware{logging}, registry))
	}
	allowed := map[string]struct{}{}
	for id := range upstreams.servers {
		allowed[id] = struct{}{}
	}
	live := &liveProxyCatalog{adapter: a, server: s, registry: registry, router: router, allowed: allowed}
	if selection.Mode == mcpgateway.Flat {
		live.addProxyTools(registry.AllDefinitions()...)
	} else {
		a.registerSearchTool(s, gateway)
		a.registerListTool(s, gateway)
		a.registerCallTool(s, gateway)
	}
	a.registerGatewayStatus(s, gateway)
	unsubscribe = r.Subscribe(func(refresh ToolRefreshResult) {
		updateMu.Lock()
		defer updateMu.Unlock()
		if closed || !upstreams.servers[refresh.ServerID] {
			return
		}
		if err := syncOrigin(refresh.ServerID); err != nil {
			a.logger().Error("daemon MCP view refresh refused", "reason", err.Error())
			return
		}
		if selection.Mode == mcpgateway.Flat {
			live.applyRefresh(refresh)
		}
	})
	success = true
	return &GatewayView{Server: s.SDKServer(), Service: gateway, close: closeView}, nil
}
