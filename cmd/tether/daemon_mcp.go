package main

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcptransport"
)

// buildDaemonMCP builds the daemon's MCP gateway. When the daemon runs a token issuer, the gateway verifies issued agent
// JWTs as well as stored credentials (the composite verifier the rest of the daemon uses); with only the stored store a
// bound agent token would be refused at /mcp whatever the listener allows.
func buildDaemonMCP(ctx context.Context, svc *app.Service, cfg daemon.Config, ids *identity.Store, issuer *authissuer.Issuer, teams api.TeamOps) (*daemonMCP, error) {
	if !cfg.Modules.Enabled(environment.LocalMCP) || !svc.Catalog.Global.Daemon.MCPEndpoint.Enabled {
		return nil, nil
	}
	var verifier identity.Verifier
	if ids != nil {
		verifier = ids
		if issuer != nil {
			composite, err := authissuer.NewVerifier(ids, issuer)
			if err != nil {
				return nil, err
			}
			verifier = composite
		}
	}
	cache := &mcptransport.CatalogCache{Root: svc.CatalogRoot}
	handlerConfig := mcptransport.HandlerConfig{
		TeamsEnabled: cfg.TeamsEnabled, Teams: teams,
		Publisher:  mcpadapter.NewDaemonToolCallRecorder(ctx, svc.Bus, svc.Store),
		ListenAddr: cfg.ListenAddr, IdentityMode: cfg.IdentityModeFor("mcp_gateway"), Verifier: verifier, Service: svc,
		NativeClientSource: func(token func() string) *client.Client {
			return client.New(cfg.ListenAddr, client.WithTokenSource(token))
		},
		Resolver: mcptransport.CallerResolver{
			Catalog:         cache.Load,
			Session:         svc.Store.SessionMCPPolicy,
			RecoverySession: svc.Store.RecoverySessionMCPPolicy,
		},
		NewRuntime: func(ctx context.Context, cat *config.Catalog) (*mcpadapter.SharedUpstreams, error) {
			entries, err := config.LoadMCPServersContext(ctx, svc.CatalogRoot)
			if err != nil {
				return nil, err
			}
			upstreams, err := mcpadapter.NewSharedUpstreams(entries, mcpadapter.DaemonProtectedRoots{
				Catalog: svc.CatalogRoot, CatalogConfig: cat, Run: filepath.Dir(cfg.PIDFile),
				State: filepath.Dir(config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)),
			}, true)
			if err != nil {
				return nil, err
			}
			upstreams.SetStateHandler(upstreamStatePublisher(ctx, svc.Bus))
			return upstreams, nil
		},
	}
	handler, err := mcptransport.NewHandler(ctx, handlerConfig)
	if err != nil {
		return nil, err
	}
	var backend *mcptransport.Backend
	if strings.HasPrefix(cfg.ListenAddr, "unix:") {
		backend, err = mcptransport.NewBackend(ctx, handlerConfig)
		if err != nil {
			handler.Close()
			return nil, err
		}
	}
	return &daemonMCP{Handler: handler, backend: backend}, nil
}

// daemonMCP owns both the existing aggregate endpoint and the additive native
// backend. Both drain before the service store closes.
type daemonMCP struct {
	*mcptransport.Handler
	backend *mcptransport.Backend
}

func (m *daemonMCP) Close() {
	if m == nil {
		return
	}
	if m.backend != nil {
		m.backend.Close()
	}
	m.Handler.Close()
}
func (m *daemonMCP) Drain(ctx context.Context) {
	if m == nil {
		return
	}
	if m.backend != nil {
		m.backend.Drain(ctx)
	}
	m.Handler.Drain(ctx)
}

func (m *daemonMCP) ToolCallRecorderStats() (uint64, uint64) {
	if m == nil {
		return 0, 0
	}
	return m.Handler.ToolCallRecorderStats()
}
