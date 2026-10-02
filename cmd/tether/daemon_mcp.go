package main

import (
	"context"
	"path/filepath"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcptransport"
)

func buildDaemonMCP(ctx context.Context, svc *app.Service, cfg daemon.Config, ids *identity.Store) (*mcptransport.Handler, error) {
	if !svc.Catalog.Global.Daemon.MCPEndpoint.Enabled {
		return nil, nil
	}
	var verifier identity.Verifier
	if ids != nil {
		verifier = ids
	}
	cache := &mcptransport.CatalogCache{Root: svc.CatalogRoot}
	return mcptransport.NewHandler(ctx, mcptransport.HandlerConfig{
		Publisher:  mcpadapter.NewDaemonToolCallRecorder(ctx, svc.Bus, svc.Store),
		ListenAddr: cfg.ListenAddr, IdentityMode: cfg.IdentityMode, Verifier: verifier, Service: svc,
		NativeClient: func(token string) *client.Client { return client.New(cfg.ListenAddr, client.WithToken(token)) },
		Resolver: mcptransport.CallerResolver{
			Catalog: cache.Load,
			Session: svc.Store.SessionMCPPolicy,
		},
		NewRuntime: func(ctx context.Context, cat *config.Catalog) (*mcpadapter.SharedUpstreams, error) {
			entries, err := config.LoadMCPServersContext(ctx, svc.CatalogRoot)
			if err != nil {
				return nil, err
			}
			return mcpadapter.NewSharedUpstreams(entries, mcpadapter.DaemonProtectedRoots{
				Catalog: svc.CatalogRoot, CatalogConfig: cat, Run: filepath.Dir(cfg.PIDFile),
				State: filepath.Dir(config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)),
			}, true)
		},
	})
}
