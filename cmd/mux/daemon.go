package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/a2aadapter"
	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/apikeyhelper"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/broker"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/federation"
	llm "github.com/hollis-labs/tether/internal/llm"
	llmanthropic "github.com/hollis-labs/tether/internal/llm/anthropic"
	llmgemini "github.com/hollis-labs/tether/internal/llm/gemini"
	"github.com/hollis-labs/tether/internal/llm/observability"
	llmopenai "github.com/hollis-labs/tether/internal/llm/openai"
	llmopenaicompat "github.com/hollis-labs/tether/internal/llm/openaicompat"
	"github.com/hollis-labs/tether/internal/llm/router"
	"github.com/hollis-labs/tether/internal/llm/secrets"
	llmservice "github.com/hollis-labs/tether/internal/llm/service"
	"github.com/hollis-labs/tether/internal/llm/usagebudget"
	"github.com/hollis-labs/tether/internal/messaging"
	"github.com/hollis-labs/tether/internal/modelcatalog"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Long-lived muxd process commands",
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the muxd daemon in the background",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadDaemonConfig(catalogPath)
		if err != nil {
			return err
		}

		// Pre-flight: reject if another daemon is already live so we don't
		// fork a child that will immediately bail with ErrAlreadyRunning.
		if pid, err := daemon.ReadPIDFile(cfg.PIDFile); err == nil && daemon.IsAlive(pid) {
			return fmt.Errorf("daemon already running (pid %d, pidfile %s)", pid, cfg.PIDFile)
		}

		// Re-exec ourselves in daemon-run mode. os.Args[0] is our own binary.
		child := exec.Command(os.Args[0], "daemon", "run", "--catalog", catalogPath) //nolint:gosec // G204: re-exec of own binary

		stateRoot := filepath.Dir(config.Expand(catalogPath))
		logFile, err := openDaemonLog(filepath.Join(stateRoot, "logs", "muxd.log"))
		if err != nil {
			log.Printf("daemon: could not open log file, continuing without file logging: %v", err)
		}
		if logFile != nil {
			child.Stdout = logFile
			child.Stderr = logFile
		}
		child.Stdin = nil
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			if logFile != nil {
				_ = logFile.Close()
			}
			return fmt.Errorf("spawn daemon: %w", err)
		}
		// The child holds the log file fd now; close the parent's copy.
		if logFile != nil {
			_ = logFile.Close()
		}
		// Detach: don't Wait() on the child. Release OS process resource.
		if err := child.Process.Release(); err != nil {
			return fmt.Errorf("release child: %w", err)
		}

		// Poll for the PID file to appear as a startup-complete signal.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if pid, err := daemon.ReadPIDFile(cfg.PIDFile); err == nil && daemon.IsAlive(pid) {
				fmt.Fprintf(os.Stderr, "muxd started\n  pid: %d\n  listener: %s\n  pidfile: %s\n",
					pid, cfg.ListenAddr, cfg.PIDFile)
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return fmt.Errorf("daemon did not write %s within 3s — check logs", cfg.PIDFile)
	},
}

var daemonRunCmd = &cobra.Command{
	Use:    "run",
	Short:  "Run the daemon in the foreground (invoked by `daemon start`; avoid calling directly)",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Pre-flight liveness check BEFORE app.New: app.New unconditionally
		// runs seedLogicalAgents (a DB write) and, on a catalog-absent
		// first run, auto-seeds catalog files; the code below additionally
		// runs ReconcileStaleState (marks any 'launching'/'running' session
		// row 'failed') and a full registry bootstrap -- all against the
		// SAME state.db a second `daemon run` invocation would share with
		// an already-live daemon. daemon.Server.Run has its own PID-file
		// liveness check, but it fires only after all of that has already
		// mutated the database (T11 durability review, CW-20260906-0042:
		// live-reproduced this exact ordering letting a second invocation
		// silently mark a live daemon's genuinely-running sessions
		// "failed" before ever discovering it couldn't actually bind).
		// This check is deliberately best-effort and duplicated here: if
		// the catalog doesn't exist yet (first-ever run), there is no PID
		// file to read and therefore nothing that could already be live
		// for this state root -- safe to fall through to the normal
		// auto-seed path in app.New below. See also
		// internal/daemon/listener_unix.go's removeStaleSocket, hardened
		// by the same review to refuse stealing a socket something is
		// still actually listening on, independent of PID-file integrity.
		if cat, cfgErr := config.Load(catalogPath); cfgErr == nil {
			if daemonCfg, err := daemonConfigFromCatalog(cat); err == nil {
				if pid, err := daemon.ReadPIDFile(daemonCfg.PIDFile); err == nil && daemon.IsAlive(pid) {
					return fmt.Errorf("%w (pid %d, pidfile %s)", daemon.ErrAlreadyRunning, pid, daemonCfg.PIDFile)
				}
			}
		}

		svc, err := app.New(catalogPath)
		if err != nil {
			return err
		}
		// Sweep stale sessions ONLY at daemon startup, never from short-
		// lived subcommands (`mux mcp`, `mux agents`, etc.) — those may
		// run concurrently with the daemon (e.g. as an MCP subprocess
		// spawned by a session) and would clobber actively-tracked rows.
		svc.ReconcileStaleState()

		// Signal-cancellable context spans the bootstrap + the HTTP serve so
		// SIGINT/SIGTERM during a slow bootstrap (large catalog, slow disk)
		// aborts cleanly instead of running to completion before shutdown.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		// Bootstrap the federation directory before the listener binds so the
		// daemon publishes a deduped, substrate-attributed /registry surface at
		// startup-complete.
		if svc.Registry != nil && svc.CatalogRoot != "" {
			report, err := svc.Registry.BootstrapFromCatalog(ctx, svc.CatalogRoot, false)
			if err != nil {
				log.Printf("registry bootstrap (tether catalog): %v", err)
			} else {
				log.Printf("registry bootstrap (tether catalog): imported=%d attached=%d skipped=%d refreshed=%d errors=%d",
					report.Imported, report.Attached, report.Skipped, report.Refreshed, len(report.Errors))
				for _, e := range report.Errors {
					log.Printf("registry bootstrap error: %s: %s", e.Path, e.Reason)
				}
			}
			if attached, err := registry.BackfillTetherExternalIDs(ctx, svc.Registry, svc.CatalogRoot); err != nil {
				log.Printf("registry bootstrap (tether external-id backfill): %v", err)
			} else {
				log.Printf("registry bootstrap (tether external-id backfill): attached=%d", attached)
			}
			report, err = registry.BootstrapFromCerberus(ctx, svc.Registry, "", false, false)
			if err != nil {
				log.Printf("registry bootstrap (cerberus): %v", err)
			} else {
				log.Printf("registry bootstrap (cerberus): imported=%d attached=%d skipped=%d refreshed=%d errors=%d",
					report.Imported, report.Attached, report.Skipped, report.Refreshed, len(report.Errors))
				for _, e := range report.Errors {
					log.Printf("registry bootstrap error: %s: %s", e.Path, e.Reason)
				}
			}
		}

		// Install the v060-05 mention parser on the registry service.
		// Parser dispatches notice envelopes to the messaging-store on
		// every successful SendToGroup. The composition order is:
		//   1. app.New constructs svc.Registry (no parser yet)
		//   2. messaging.NewParser binds Registry (for resolution) +
		//      MessagingStore (for emission)
		//   3. SetMentionParser installs the hook before HTTP serves
		// — so the daemon's group-send path always has the parser
		// attached. Without this wiring, the daemon still accepts group
		// sends but mentions never produce notices.
		//
		// *registry.Service satisfies messaging.Lookup directly
		// (exposes Lookup + FindByDisplayName); no adapter needed.
		if svc.Registry != nil {
			parser := messaging.NewParser(svc.Registry, svc.Store.MessagingStore())
			svc.Registry.SetMentionParser(parser)
		}

		cfg, err := daemonConfigFromCatalog(svc.Catalog)
		if err != nil {
			_ = svc.Store.Close()
			return err
		}
		aiSvc := buildAIServiceFromConfig(ctx, svc.Catalog, aiServiceDeps{
			Recorder:  svc.Store,
			Usage:     svc.Store,
			Publisher: svc.Bus,
		})

		a2aHandler, err := buildA2AAdapter(svc.CatalogRoot, svc.Store.MessagingStore())
		if err != nil {
			return err
		}

		stateRoot := filepath.Dir(config.Expand(catalogPath))
		server := &daemon.Server{
			Config:              cfg,
			Manager:             svc.Manager,
			Service:             &serviceAdapter{svc: svc},
			AI:                  aiSvc,
			AIAudit:             svc.Store,
			AIUsage:             svc.Store,
			Checkpoints:         svc.Store,
			Broker:              &brokerAdapter{write: svc.Broker, read: svc.Store},
			Bus:                 svc.Bus,
			EventsStore:         svc.Store,
			Catalog:             &catalogLoader{root: svc.CatalogRoot},
			GroupStore:          svc.Store,
			Workstreams:         svc.Store,
			SessionRefs:         svc.Store,
			Digests:             svc.Store,
			MessageStore:        newFederatedMessageStore(svc.Store.MessagingStore(), svc.Federation),
			DeliveryClaims:      svc.Store,
			Attachments:         svc.Store,
			ProxyEvents:         svc.Store,
			Registry:            svc.Registry,
			RegistryCatalogRoot: svc.CatalogRoot,
			Groups:              svc.Registry,
			Publisher:           svc.Bus,
			WakeSweeper:         svc,
			LogsDir:             filepath.Join(stateRoot, "logs"),
			SessionBootstrap:    svc.Store,
			DeliveryTrace:       svc.Store,
			DeliveryRepair:      svc.Store,
			Retention:           svc.Store,
			A2A:                 a2aHandler,
			Close: func() error {
				// Manager.Shutdown is driven by daemon.Server; Close just
				// releases the store handle so the process can exit cleanly.
				return svc.Store.Close()
			},
		}

		return server.Run(ctx)
	},
}

// buildA2AAdapter loads the opt-in A2A binding catalog (T10, messaging
// vNext) and constructs the adapter's HTTP handler. Returns a nil handler
// (not an error) when no bindings are configured -- "the feature stays
// optional for local messaging" (T10 acceptance #3): an empty/missing
// <catalogRoot>/a2a/ directory means the A2A surface doesn't exist on
// this daemon at all, matching the daemon.Server.A2A field's nil-means-
// absent convention.
func buildA2AAdapter(catalogRoot string, sender a2aadapter.MessageSender) (http.Handler, error) {
	entries, err := config.LoadA2ABindings(catalogRoot)
	if err != nil {
		return nil, fmt.Errorf("load a2a bindings: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil
	}

	bindings := make([]a2aadapter.AgentBinding, len(entries))
	for i, e := range entries {
		bindings[i] = a2aadapter.AgentBinding{
			ID:               e.ID,
			TargetURN:        e.TargetURN,
			DisplayName:      e.DisplayName,
			Description:      e.Description,
			BaseURL:          e.BaseURL,
			BearerToken:      e.BearerToken,
			TaskMode:         e.TaskMode,
			TaskAwaitTimeout: e.TaskAwaitTimeout(),
		}
	}
	adapter, err := a2aadapter.NewAdapter(a2aadapter.Config{Bindings: bindings}, sender)
	if err != nil {
		return nil, fmt.Errorf("build a2a adapter: %w", err)
	}
	return adapter.Mux(), nil
}

func buildAIServiceFromConfig(ctx context.Context, cat *config.Catalog, deps aiServiceDeps) api.AIService {
	if cat == nil {
		return nil
	}
	helperPath := apikeyhelper.ResolvePath()
	if helperPath == "" {
		log.Printf("ai gateway disabled: mux-apikey-helper not found")
		return nil
	}

	catalog := modelcatalog.New()
	if err := catalog.Refresh(ctx); err != nil {
		log.Printf("ai gateway disabled: refresh model catalog: %v", err)
		return nil
	}

	ai := cat.Global.AI
	if len(ai.Providers) == 0 {
		return nil
	}

	secretResolver := secrets.NewResolver(
		secrets.WithDefaultHelperPath(helperPath),
		secrets.WithNamedHelperResolver(apikeyhelper.ResolveNamedPath),
	)

	providers := map[string]llm.ChatProvider{}
	providerInfos := map[string]llmservice.ProviderInfo{}
	providerConfigs := map[string]config.AIProviderConfig{}
	var routes []router.Route
	for _, p := range ai.Providers {
		if !p.Enabled {
			continue
		}
		providerConfigs[p.ID] = p
		switch p.Type {
		case "anthropic":
			secretRef := p.SecretRef
			providers[p.ID] = llmanthropic.New(llmanthropic.Config{
				BaseURL: p.BaseURL,
				ResolveAPIKey: func(ctx context.Context) (string, error) {
					return resolveAISecret(ctx, secretResolver, secretRef)
				},
			})
			providerInfos[p.ID] = llmservice.ProviderInfo{
				ID:           p.ID,
				Type:         p.Type,
				DefaultModel: p.EffectiveDefaultModel(),
				Models:       p.EffectiveModels(),
				BaseURL:      p.BaseURL,
			}
		case "openai":
			secretRef := p.SecretRef
			providers[p.ID] = llmopenai.New(llmopenai.Config{
				BaseURL: p.BaseURL,
				ResolveAPIKey: func(ctx context.Context) (string, error) {
					return resolveAISecret(ctx, secretResolver, secretRef)
				},
			})
			providerInfos[p.ID] = llmservice.ProviderInfo{
				ID:           p.ID,
				Type:         p.Type,
				DefaultModel: p.EffectiveDefaultModel(),
				Models:       p.EffectiveModels(),
				BaseURL:      p.BaseURL,
			}
		case "gemini":
			secretRef := p.SecretRef
			providers[p.ID] = llmgemini.New(llmgemini.Config{
				BaseURL: p.BaseURL,
				ResolveAPIKey: func(ctx context.Context) (string, error) {
					return resolveAISecret(ctx, secretResolver, secretRef)
				},
			})
			providerInfos[p.ID] = llmservice.ProviderInfo{
				ID:           p.ID,
				Type:         p.Type,
				DefaultModel: p.EffectiveDefaultModel(),
				Models:       p.EffectiveModels(),
				BaseURL:      p.BaseURL,
			}
		case "openai-compatible":
			var resolve func(context.Context) (string, error)
			if p.SecretRef != "" {
				secretRef := p.SecretRef
				resolve = func(ctx context.Context) (string, error) {
					return resolveAISecret(ctx, secretResolver, secretRef)
				}
			}
			providers[p.ID] = llmopenaicompat.New(llmopenaicompat.Config{
				BaseURL:       p.BaseURL,
				ResolveAPIKey: resolve,
			})
			providerInfos[p.ID] = llmservice.ProviderInfo{
				ID:           p.ID,
				Type:         p.Type,
				DefaultModel: p.EffectiveDefaultModel(),
				Models:       p.EffectiveModels(),
				BaseURL:      p.BaseURL,
			}
		default:
			log.Printf("ai gateway: skipping unsupported provider type %q for %s", p.Type, p.ID)
		}
	}
	if len(providers) == 0 {
		return nil
	}

	order := ai.Routing.DefaultProviderOrder
	if len(order) == 0 {
		for _, p := range ai.Providers {
			if p.Enabled {
				order = append(order, p.ID)
			}
		}
	}
	if len(ai.Routing.Routes) > 0 {
		for _, routeCfg := range ai.Routing.Routes {
			providerCfg, ok := providerConfigs[routeCfg.Provider]
			if !ok {
				continue
			}
			allowReasoning, allowTools, allowAttachments := effectiveAIRoutePolicy(ai.Policy, providerCfg.Policy, routeCfg)
			usageBudget := effectiveAIUsageBudget(ai.Policy.UsageBudget, providerCfg.Policy.UsageBudget, routeCfg.UsageBudget)
			routes = append(routes, router.Route{
				Provider:          routeCfg.Provider,
				CatalogProvider:   modelCatalogProviderID(providerCfg.Type),
				Model:             routeCfg.Model,
				Mode:              routeCfg.Mode,
				Intent:            routeCfg.Intent,
				RequiresReasoning: routeCfg.RequiresReasoning,
				RequiresTools:     routeCfg.RequiresTools,
				AllowReasoning:    allowReasoning,
				AllowTools:        allowTools,
				AllowAttachments:  allowAttachments,
				MaxOutputTokens:   coalesceInt(routeCfg.MaxOutputTokens, coalesceInt(providerCfg.Policy.MaxOutputTokens, ai.Policy.MaxOutputTokens)),
				MaxCostUSD:        coalesceFloat64(routeCfg.MaxCostUSD, coalesceFloat64(providerCfg.Policy.MaxCostUSD, ai.Policy.MaxCostUSD)),
				UsageBudget:       usageBudget,
			})
		}
	} else {
		for _, id := range order {
			p, ok := providerConfigs[id]
			if !ok {
				continue
			}
			if _, ok := providers[id]; !ok {
				continue
			}
			usageBudget := effectiveAIDefaultUsageBudget(ai.Policy.UsageBudget, p.Policy.UsageBudget)
			for _, model := range p.EffectiveModels() {
				allowReasoning, allowTools, allowAttachments := effectiveAIDefaultPolicy(ai.Policy, p.Policy)
				routes = append(routes, router.Route{
					Provider:         id,
					CatalogProvider:  modelCatalogProviderID(p.Type),
					Model:            model,
					AllowReasoning:   allowReasoning,
					AllowTools:       allowTools,
					AllowAttachments: allowAttachments,
					MaxOutputTokens:  coalesceInt(p.Policy.MaxOutputTokens, ai.Policy.MaxOutputTokens),
					MaxCostUSD:       coalesceFloat64(p.Policy.MaxCostUSD, ai.Policy.MaxCostUSD),
					UsageBudget:      usageBudget,
				})
			}
		}
	}
	if len(routes) == 0 {
		return nil
	}

	catalogView := modelcatalog.NewOverlay(catalog, syntheticConfiguredModels(providerConfigs))

	var evaluators []router.PolicyEvaluator
	if deps.Usage != nil {
		evaluators = append(evaluators, usagebudget.Evaluator{Store: deps.Usage})
	}
	planner := router.NewWithEvaluators(catalogView, router.Policy{
		Version: "catalog-ai-v1",
		Routes:  routes,
	}, evaluators...)
	return &llmservice.Service{
		Planner:      planner,
		Catalog:      catalogView,
		Providers:    providers,
		ProviderInfo: providerInfos,
		Routes:       append([]router.Route(nil), routes...),
		RouteOrder:   append([]string(nil), order...),
		Recorder:     deps.Recorder,
		Publisher:    deps.Publisher,
	}
}

func syntheticConfiguredModels(providers map[string]config.AIProviderConfig) map[string]modelsdev.Model {
	out := make(map[string]modelsdev.Model)
	for _, p := range providers {
		providerID := modelCatalogProviderID(p.Type)
		for _, modelID := range p.EffectiveModels() {
			key := providerID + "\x00" + modelID
			out[key] = modelsdev.Model{
				ID:     modelID,
				Name:   modelID,
				Family: providerID,
				Modality: modelsdev.Modality{
					Input:  []string{"text"},
					Output: []string{"text"},
				},
				Capabilities: modelsdev.Capabilities{},
			}
		}
	}
	return out
}

func modelCatalogProviderID(providerType string) string {
	switch providerType {
	case "openai", "openai-compatible":
		return "openai"
	case "gemini":
		return "google"
	default:
		return providerType
	}
}

func resolveAISecret(ctx context.Context, resolver *secrets.Resolver, ref string) (string, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return resolver.Resolve(resolveCtx, ref)
}

func effectiveAIDefaultPolicy(global, provider config.AIPolicyConfig) (*bool, *bool, *bool) {
	return coalesceBool(provider.AllowReasoning, global.AllowReasoning),
		coalesceBool(provider.AllowTools, global.AllowTools),
		coalesceBool(provider.AllowAttachments, global.AllowAttachments)
}

func effectiveAIRoutePolicy(global, provider config.AIPolicyConfig, route config.AIRouteConfig) (*bool, *bool, *bool) {
	allowReasoning, allowTools, allowAttachments := effectiveAIDefaultPolicy(global, provider)
	return coalesceBool(route.AllowReasoning, allowReasoning),
		coalesceBool(route.AllowTools, allowTools),
		coalesceBool(route.AllowAttachments, allowAttachments)
}

func effectiveAIDefaultUsageBudget(global, provider config.AIUsageBudgetPolicyConfig) router.UsageBudgetPolicy {
	level := ""
	switch {
	case hasUsageBudget(provider):
		level = "provider"
	case hasUsageBudget(global):
		level = "global"
	}
	return router.UsageBudgetPolicy{
		Level:      level,
		MaxCostUSD: coalesceFloat64(provider.MaxCostUSD, global.MaxCostUSD),
		Window:     coalesceString(provider.Window, global.Window),
		Scope:      coalesceString(provider.Scope, global.Scope),
	}
}

func effectiveAIUsageBudget(global, provider, route config.AIUsageBudgetPolicyConfig) router.UsageBudgetPolicy {
	level := ""
	switch {
	case hasUsageBudget(route):
		level = "route"
	case hasUsageBudget(provider):
		level = "provider"
	case hasUsageBudget(global):
		level = "global"
	}
	return router.UsageBudgetPolicy{
		Level:      level,
		MaxCostUSD: coalesceFloat64(route.MaxCostUSD, coalesceFloat64(provider.MaxCostUSD, global.MaxCostUSD)),
		Window:     coalesceString(route.Window, coalesceString(provider.Window, global.Window)),
		Scope:      coalesceString(route.Scope, coalesceString(provider.Scope, global.Scope)),
	}
}

func coalesceBool(primary, fallback *bool) *bool {
	if primary != nil {
		return primary
	}
	return fallback
}

func coalesceInt(primary, fallback *int) *int {
	if primary != nil {
		return primary
	}
	return fallback
}

func coalesceFloat64(primary, fallback *float64) *float64 {
	if primary != nil {
		return primary
	}
	return fallback
}

func coalesceString(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

func hasUsageBudget(cfg config.AIUsageBudgetPolicyConfig) bool {
	return cfg.MaxCostUSD != nil || cfg.Window != "" || cfg.Scope != ""
}

type llmobsDeps interface {
	RecordAIAuditEvent(ev observability.AuditEvent) error
}

type aiServiceDeps struct {
	Recorder  llmobsDeps
	Usage     usagebudget.UsageStore
	Publisher events.Publisher
}

// serviceAdapter bridges *app.Service to api.LaunchService. Flattens
// app.Launched's *workspace.Session pointer into primitive strings so
// the API response never carries internal types.
type serviceAdapter struct {
	svc *app.Service
}

func (a *serviceAdapter) CreateSession(launchID string) (api.LaunchResult, error) {
	l, err := a.svc.CreateSession(launchID)
	if err != nil {
		return api.LaunchResult{}, err
	}
	return api.LaunchResult{
		SessionID:      l.SessionID,
		Workspace:      l.Workspace.Root,
		LogPath:        l.Workspace.LogPath,
		ProviderID:     l.Plan.ProviderID,
		ProviderKind:   l.ProviderKind,
		LogicalAgentID: l.Plan.LogicalAgentID,
	}, nil
}

func (a *serviceAdapter) CreateSessionWithBootPrompt(launchID, bootPrompt string) (api.LaunchResult, error) {
	l, err := a.svc.CreateSessionWithBootPrompt(launchID, bootPrompt)
	if err != nil {
		return api.LaunchResult{}, err
	}
	return api.LaunchResult{
		SessionID:      l.SessionID,
		Workspace:      l.Workspace.Root,
		LogPath:        l.Workspace.LogPath,
		ProviderID:     l.Plan.ProviderID,
		ProviderKind:   l.ProviderKind,
		LogicalAgentID: l.Plan.LogicalAgentID,
	}, nil
}

func (a *serviceAdapter) CreateSessionWithInput(in api.CreateSessionInput) (api.LaunchResult, error) {
	l, err := a.svc.CreateSessionWithInput(app.CreateSessionInput{
		LaunchID:           in.LaunchID,
		BootPromptOverride: in.BootPromptOverride,
		AgentFile:          in.AgentFile,
		AgentInline:        in.AgentInline,
		BootProfileFile:    in.BootProfileFile,
		Override:           in.Override,
		BootPromptAppend:   in.PromptAppend,
		Injection:          in.Injection,
	})
	if err != nil {
		return api.LaunchResult{}, err
	}
	return api.LaunchResult{
		SessionID:      l.SessionID,
		Workspace:      l.Workspace.Root,
		LogPath:        l.Workspace.LogPath,
		ProviderID:     l.Plan.ProviderID,
		ProviderKind:   l.ProviderKind,
		LogicalAgentID: l.Plan.LogicalAgentID,
	}, nil
}

func (a *serviceAdapter) LaunchSession(sessionID string) (api.LaunchResult, error) {
	l, err := a.svc.LaunchSession(sessionID)
	if err != nil {
		return api.LaunchResult{}, err
	}
	return api.LaunchResult{
		SessionID:      l.SessionID,
		Workspace:      l.Workspace.Root,
		LogPath:        l.Workspace.LogPath,
		ProviderID:     l.Plan.ProviderID,
		ProviderKind:   l.ProviderKind,
		LogicalAgentID: l.Plan.LogicalAgentID,
	}, nil
}

func (a *serviceAdapter) ListSessions(opts store.ListSessionsOptions) ([]store.SessionRow, error) {
	return a.svc.ListSessions(opts)
}

func (a *serviceAdapter) GetSession(id string) (*store.SessionRow, error) {
	return a.svc.GetSession(id)
}

func (a *serviceAdapter) StopSession(id string) error {
	return a.svc.StopSession(id)
}

func (a *serviceAdapter) WaitSession(ctx context.Context, id string) (int, error) {
	return a.svc.WaitSession(ctx, id)
}

func (a *serviceAdapter) SendInput(id string, data []byte) error {
	return a.svc.SendInput(id, data)
}

func (a *serviceAdapter) SendTurn(ctx context.Context, id, text string) error {
	return a.svc.SendTurn(ctx, id, text)
}

func (a *serviceAdapter) ResizeSession(id string, rows, cols uint16) error {
	return a.svc.ResizeSession(id, rows, cols)
}

func (a *serviceAdapter) AttachSession(ctx context.Context, id string, w io.Writer, sinceSeq int64) error {
	return a.svc.AttachSession(ctx, id, w, sinceSeq)
}

func (a *serviceAdapter) AttachedClients(id string) int {
	return a.svc.AttachedClients(id)
}

func (a *serviceAdapter) ResumeLogicalAgent(logicalAgentID string) (api.LaunchResult, error) {
	return a.svc.ResumeLogicalAgent(logicalAgentID)
}

func (a *serviceAdapter) GetLogicalAgentPolicy(logicalAgentID string) (agent.LogicalAgentPolicy, error) {
	return a.svc.GetLogicalAgentPolicy(logicalAgentID)
}

func (a *serviceAdapter) UpdateLogicalAgentPolicy(policy agent.LogicalAgentPolicy) (agent.LogicalAgentPolicy, error) {
	return a.svc.UpdateLogicalAgentPolicy(policy)
}

func (a *serviceAdapter) RuntimeHealth(id string) (api.RuntimeHealthResult, bool) {
	return a.svc.RuntimeHealth(id)
}

func (a *serviceAdapter) ResolveActorSession(ctx context.Context, logicalAgentID string) (string, error) {
	return a.svc.ResolveActorSession(ctx, logicalAgentID)
}

func (a *serviceAdapter) AttemptWake(ctx context.Context, messageID string, to gomsg.Address, sessionID, wakeText string) api.WakeOutcome {
	return a.svc.AttemptWake(ctx, messageID, to, sessionID, wakeText)
}

// newFederatedMessageStore is the T05 (messaging vNext) fix for the gap
// ADR-0040 itself documented: "the Router is composed onto
// Service.Federation but the MCP/HTTP message front-ends still call
// Store.MessagingStore() directly." When router is nil (the default,
// standalone, non-federated configuration -- federation.BuildRouter
// returns nil when the config's `enabled` is false), this returns local
// unchanged: zero behavior change for every existing non-federated
// install. When router is non-nil, the four authority-routable
// messaging.Store methods (Send/Inbox/Subscribe/Consume) are dispatched
// through it so a message addressed to a configured peer authority
// actually crosses hosts; List/MarkRead/Archive/Unarchive (InboxStore's
// superset, which Router does not implement -- it only satisfies the
// 7-method messaging.Store) and Get/Thread/Cancel (which Router itself
// unconditionally delegates to its local store anyway, per its own docs)
// are served directly from local, avoiding a redundant hop.
func newFederatedMessageStore(local store.InboxStore, router *federation.Router) store.InboxStore {
	if router == nil {
		return local
	}
	return &federatedMessageStore{InboxStore: local, router: router}
}

type federatedMessageStore struct {
	store.InboxStore
	router *federation.Router
}

func (f *federatedMessageStore) Send(ctx context.Context, env gomsg.Envelope) (gomsg.Envelope, error) {
	return f.router.Send(ctx, env)
}
func (f *federatedMessageStore) Inbox(ctx context.Context, to gomsg.Address, filt gomsg.Filter) ([]gomsg.Envelope, error) {
	return f.router.Inbox(ctx, to, filt)
}
func (f *federatedMessageStore) Subscribe(ctx context.Context, to gomsg.Address, filt gomsg.Filter) (<-chan gomsg.Envelope, error) {
	return f.router.Subscribe(ctx, to, filt)
}
func (f *federatedMessageStore) Consume(ctx context.Context, id string, recipient gomsg.Address) error {
	return f.router.Consume(ctx, id, recipient)
}

// catalogLoader is the production api.CatalogLoader: each Load call
// re-reads the catalog root with config.LoadLayered, so live YAML edits —
// including agents in the user/project discovery layers — are picked up
// without a daemon restart. The read cost is trivial (O(100s) YAML files at
// most) and matches ADR 0012's fresh-read stance.
type catalogLoader struct {
	root string
}

func (c *catalogLoader) Load() (*config.Catalog, error) {
	return config.LoadLayered(c.root)
}

// brokerAdapter bundles broker.Service (writes with event emission)
// and the store's envelope-read methods into the single
// api.BrokerService seam. Kept at the cmd layer because this is where
// the two halves are naturally composed (app.Service already owns both
// dependencies).
type brokerAdapter struct {
	write *broker.Service
	read  envelopeReader
}

// envelopeReader is the narrow read-side contract the adapter needs.
// *store.Store satisfies it.
type envelopeReader interface {
	GetEnvelope(id string) (*broker.Envelope, error)
	ListEnvelopesByRecipient(recipient string) ([]broker.Envelope, error)
	ListEnvelopesByWorkflow(workflowID, correlationID string) ([]broker.Envelope, error)
}

func (a *brokerAdapter) CreateEnvelope(ctx context.Context, e broker.Envelope) error {
	return a.write.CreateEnvelope(ctx, e)
}
func (a *brokerAdapter) ReplyEnvelope(ctx context.Context, reply broker.Envelope) error {
	return a.write.ReplyEnvelope(ctx, reply)
}
func (a *brokerAdapter) GetEnvelope(id string) (*broker.Envelope, error) {
	return a.read.GetEnvelope(id)
}
func (a *brokerAdapter) ListEnvelopesByRecipient(recipient string) ([]broker.Envelope, error) {
	return a.read.ListEnvelopesByRecipient(recipient)
}
func (a *brokerAdapter) WaitForResponse(ctx context.Context, correlationID string) (*broker.Envelope, error) {
	return a.write.WaitForResponse(ctx, correlationID)
}

func (a *brokerAdapter) ListEnvelopesByWorkflow(workflowID, correlationID string) ([]broker.Envelope, error) {
	return a.read.ListEnvelopesByWorkflow(workflowID, correlationID)
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the muxd daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadDaemonConfig(catalogPath)
		if err != nil {
			return err
		}
		pid, err := daemon.ReadPIDFile(cfg.PIDFile)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "muxd not running (no pid file)")
				return nil
			}
			return err
		}
		if !daemon.IsAlive(pid) {
			fmt.Fprintf(os.Stderr, "muxd not running; removing stale pidfile %s\n", cfg.PIDFile)
			return daemon.RemovePIDFile(cfg.PIDFile)
		}

		proc, err := os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("find process %d: %w", pid, err)
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("sigterm pid %d: %w", pid, err)
		}

		// Poll: daemon removes its own PID file on clean shutdown.
		deadline := time.Now().Add(cfg.ShutdownTimeout + 2*time.Second)
		for time.Now().Before(deadline) {
			if !daemon.IsAlive(pid) {
				fmt.Fprintf(os.Stderr, "muxd stopped (pid %d)\n", pid)
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("muxd pid %d did not exit within %s", pid, cfg.ShutdownTimeout+2*time.Second)
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show muxd daemon status",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadDaemonConfig(catalogPath)
		if err != nil {
			return err
		}
		pid, err := daemon.ReadPIDFile(cfg.PIDFile)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "muxd: not running")
				os.Exit(1)
			}
			return err
		}
		if !daemon.IsAlive(pid) {
			fmt.Fprintf(os.Stderr, "muxd: stale pid %d in %s\n", pid, cfg.PIDFile)
			os.Exit(2)
		}

		client := daemon.DialHTTPClient(cfg.ListenAddr)
		resp, err := client.Get(daemon.BaseURL(cfg.ListenAddr) + "/health")
		if err != nil {
			// Daemon is alive per PID but not responding — partial outage.
			fmt.Fprintf(os.Stderr, "muxd: pid %d alive but /health unreachable: %v\n", pid, err)
			os.Exit(3)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("health status %d", resp.StatusCode)
		}
		var h daemon.Health
		if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
			return fmt.Errorf("decode /health: %w", err)
		}
		fmt.Printf("pid:      %d\nuptime:   %ds\nlistener: %s\nsessions: %d\n",
			h.PID, h.UptimeSec, h.Listener, h.Sessions)
		return nil
	},
}

// newDaemonClient returns a client wired at the catalog's daemon listen
// address. Shared by the launch / sessions subcommands so the daemon
// transport lives in one place.
func newDaemonClient(catalogRoot string) (*client.Client, error) {
	cfg, err := loadDaemonConfig(catalogRoot)
	if err != nil {
		return nil, err
	}
	return client.New(cfg.ListenAddr), nil
}

// openStoreReadOnly opens the SQLite store for fallback reads when the
// daemon is unreachable. modernc.org/sqlite supports multi-reader
// concurrency so this is safe even if the daemon has the file open too.
// Caller is responsible for closing the returned *store.Store.
func openStoreReadOnly(catalogRoot string) (*store.Store, error) {
	cat, err := config.Load(catalogRoot)
	if err != nil {
		return nil, err
	}
	// Explicit catalog state_db wins; cat.Paths supplies the go-apppaths
	// fallback only when global.yaml omits the key.
	dbPath := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
	if dbPath == "" {
		return nil, fmt.Errorf("global.defaults.state_db missing")
	}
	return store.Open(dbPath)
}

// loadDaemonConfig loads just enough of the catalog to resolve daemon
// paths. Used by start/stop/status so they don't open the SQLite store.
func loadDaemonConfig(catalogRoot string) (daemon.Config, error) {
	cat, err := config.Load(catalogRoot)
	if err != nil {
		return daemon.Config{}, err
	}
	return daemonConfigFromCatalog(cat)
}

func daemonConfigFromCatalog(cat *config.Catalog) (daemon.Config, error) {
	d := cat.Global.Daemon
	timeout, err := time.ParseDuration(d.ShutdownTimeout)
	if err != nil {
		return daemon.Config{}, fmt.Errorf("parse daemon.shutdown_timeout %q: %w", d.ShutdownTimeout, err)
	}
	return daemon.Config{
		ListenAddr:      expandListenAddr(d.ListenAddr),
		PIDFile:         config.Expand(d.PIDFile),
		ShutdownTimeout: timeout,
	}, nil
}

// openDaemonLog creates the logs directory and opens (or creates+appends)
// muxd.log with simple size-based rotation. Returns nil on any error so the
// caller can degrade gracefully (log to stderr) rather than failing the spawn.
//
// Rotation: if the current log exceeds 10 MiB, shift generations up to
// muxd.log.3 (dropping the oldest) before opening a fresh muxd.log.
func openDaemonLog(logPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return nil, err
	}
	rotateDaemonLog(logPath, 10<<20, 3) // 10 MiB, keep 3 generations
	//nolint:gosec // G304: path derived from tether state root, not user input.
	return os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
}

// rotateDaemonLog renames logPath → logPath.1 → … → logPath.N when
// logPath's size exceeds maxBytes, dropping generation N+1 if present.
func rotateDaemonLog(logPath string, maxBytes int64, generations int) {
	info, err := os.Stat(logPath)
	if err != nil || info.Size() <= maxBytes {
		return
	}
	for g := generations; g >= 1; g-- {
		older := fmt.Sprintf("%s.%d", logPath, g)
		newer := fmt.Sprintf("%s.%d", logPath, g-1)
		if g == 1 {
			newer = logPath
		}
		if _, err := os.Stat(newer); err == nil {
			_ = os.Rename(newer, older)
		}
	}
}

// expandListenAddr runs config.Expand on the path portion of a unix: addr;
// tcp: addrs are untouched (host:port isn't path-like).
func expandListenAddr(addr string) string {
	const pfx = "unix:"
	if len(addr) > len(pfx) && addr[:len(pfx)] == pfx {
		return pfx + config.Expand(addr[len(pfx):])
	}
	return addr
}

func init() {
	daemonCmd.AddCommand(daemonStartCmd, daemonRunCmd, daemonStopCmd, daemonStatusCmd)
}
