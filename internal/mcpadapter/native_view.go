package mcpadapter

import (
	"context"
	"fmt"
	"sync"

	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewNativeView exposes Tether's native handlers to a co-located gateway. There
// is no SharedUpstreams, subprocess runtime, aggregate discovery dispatcher or
// refresh handler. A partial native inventory must not reject profile pins for
// other origins; the transport validates selectors against the full catalog.
func (a *Adapter) NewNativeView(ctx context.Context, opts ProxyOptions) (*GatewayView, error) {
	if a == nil || a.principal == nil {
		return nil, fmt.Errorf("verified native MCP adapter required")
	}
	for _, selection := range append([]mcpgateway.ProfileSelection{opts.Profile}, opts.AuthorityProfiles...) {
		if selection.Profile != nil {
			if err := selection.Profile.Validate(); err != nil {
				return nil, err
			}
			for _, names := range [][]string{selection.Profile.Order, selection.Profile.AlwaysLoad, selection.Profile.Pinned} {
				for _, name := range names {
					if mcpgateway.IsInfrastructure(name) {
						return nil, fmt.Errorf("profile cannot pin gateway infrastructure %q", name)
					}
				}
			}
		}
	}
	for _, floor := range opts.AuthorityProfiles {
		if floor.Profile == nil {
			return nil, mcpgateway.ErrInvalidSessionMCPPolicy
		}
	}
	native := a.newServer()
	if opts.ProxyStore != nil {
		a.registerToolCallEventsTool(native, opts.ProxyStore)
	}
	// Result paging belongs to the outer gateway view; a second cursor namespace
	// here would make continuation ambiguous. Handler-owned paging is unchanged.
	native.RemoveTools("tether_result_next")
	st, ct := mcpsdk.NewInMemoryTransports()
	nativeServer, err := native.SDKServer().Connect(ctx, st, nil)
	if err != nil {
		return nil, err
	}
	nativeClient, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "tether-native-backend", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		_ = nativeServer.Close()
		return nil, err
	}
	s := a.newBareServer()
	var once sync.Once
	closeView := func() {
		once.Do(func() {
			for session := range s.SDKServer().Sessions() {
				_ = session.Close()
			}
			_ = nativeClient.Close()
			_ = nativeServer.Close()
		})
	}
	success := false
	defer func() {
		if !success {
			closeView()
		}
	}()
	registry := NewToolRegistry()
	for tool, err := range nativeClient.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		if err := registry.RegisterLocal(tool, nativeClient); err != nil {
			return nil, err
		}
	}
	router := NewProxyRouter(registry)
	router.SetLogger(a.logger())
	if a.readsViaDaemon() || (a.svc != nil && a.svc.Store != nil) {
		router.SetWorkstreamResolver(a.sessionWorkstreamID)
	}
	// The backend always supplies concrete native definitions; search/flat is the
	// standalone gateway's presentation decision, not an alternate authority.
	gateway := a.gatewayService(registry, router, mcpgateway.Selection{Mode: mcpgateway.Flat, Source: "native backend"}, nil)
	gateway.Policy = &mcpgateway.Policy{Selection: opts.Profile, Floors: opts.AuthorityProfiles, ServerOrder: []string{"tether"}}
	s.SDKServer().AddReceivingMiddleware(gatewaySurfaceMiddleware(gateway))
	a.registerDocsResources(s)
	s.SDKServer().AddReceivingMiddleware(docsResourceMiddleware(gateway))
	mws := []ToolCallMiddleware{&MachineAuthorityMiddleware{pub: opts.Publisher, Access: opts.MachineAccess, registry: registry}}
	if opts.Publisher != nil {
		logging := NewLoggingMiddleware(opts.Publisher)
		logging.profile, logging.mode = opts.Profile.ID, "native"
		logging.contextDecorator = a.withSessionID
		mws = append(mws, logging)
	}
	s.SDKServer().AddReceivingMiddleware(proxyLoggingMiddleware(mws, registry))
	live := &liveProxyCatalog{adapter: a, server: s, registry: registry, router: router}
	live.addProxyTools(registry.AllDefinitions()...)
	success = true
	return &GatewayView{Server: s.SDKServer(), Service: gateway, close: closeView}, nil
}
