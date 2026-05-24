// Package app composes the catalog, store, event bus, broker, and
// agentsessions.Manager into a single Service that the daemon, CLI, MCP,
// and ACP adapters share. Per ADR 0002 and the boot-prompt invariants,
// internal/app stays thin: this file is the composition root only. Per-
// domain methods (session lifecycle, I/O, catalog reads, resume, codex
// JSON-RPC turn delivery) live in sibling files keeping each one well
// below the ~300 LOC ceiling.
package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/go-agent-runtime/turn"
	"github.com/hollis-labs/go-agent-sessions/agentsessions"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/broker"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/federation"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/specresolve"
	"github.com/hollis-labs/tether/internal/store"
)

// RuntimeFactory builds an agentsessions.Runtime for a single launch. The
// plan supplies binary path, args, and env policy; per-launch StartOptions
// supply workspace, log path, sandbox profile, and fanout (passed at
// Manager.Start time).
type RuntimeFactory func(plan *launch.Plan) (agentsessions.Runtime, error)

// Service is the composition root: it wires catalog, store, sinks, and
// the agentsessions.Manager. Lifecycle ownership of running sessions
// lives in agentsessions; Service only orchestrates launch preconditions
// (plan resolution, workspace creation, persistence of the initial row)
// and then hands the handle off to the manager.
type Service struct {
	CatalogRoot string
	Catalog     *config.Catalog
	Store       *store.Store
	Manager     *agentsessions.Manager
	Bus         events.Bus
	Broker      *broker.Service

	// Federation is the authority-routing messaging Router, or nil when
	// federation is disabled (the standalone default). When non-nil it
	// decorates the local messaging store so envelopes addressed to a
	// configured peer authority route cross-host. See internal/federation.
	Federation *federation.Router

	// Registry is the v0.6 federation directory service (registry +
	// search + sync). Populated by daemon startup wiring; nil in lighter
	// composition contexts (e.g. read-only `mux mcp` connecting to a
	// remote daemon for session ops). MCP / HTTP / CLI surfaces that
	// depend on it must nil-guard before dispatching.
	Registry *registry.Service

	factories map[string]RuntimeFactory

	// codexThreads caches Codex app-server JSON-RPC thread state per
	// Tether session id. Populated lazily on the first SendTurn call.
	codexThreads turn.CodexAppServerCache

	// specResolver is the S5 Spec-path launch resolver. It is constructed
	// lazily on first use (only when the launch engine is "spec") via
	// specResolverOnce and reused across launches. specResolverErr records
	// a construction failure so it surfaces on every callsite, not just
	// the first. See launch_engine.go and specResolverFor.
	specResolverOnce sync.Once
	specResolver     *specresolve.Resolver
	specResolverErr  error
}

// New constructs a Service rooted at catalogRoot. Reads + validates the
// catalog, opens the SQLite store at the catalog's configured path, seeds
// logical_agents from catalog agents, wires the agentsessions.Manager
// with state/attachment/event sinks, and registers the built-in + catalog-
// declared runtime factories.
func New(catalogRoot string) (*Service, error) {
	cat, err := config.LoadLayered(catalogRoot)
	if err != nil {
		return nil, err
	}
	if err := cat.Validate(); err != nil {
		return nil, err
	}
	// Explicit catalog state_db wins; the go-apppaths Layout (cat.Paths)
	// supplies the fallback only when global.yaml omits the key.
	dbPath := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
	if dbPath == "" {
		return nil, fmt.Errorf("global.defaults.state_db missing")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}

	factories := map[string]RuntimeFactory{}
	for _, p := range cat.Providers {
		factory, err := runtimeFactoryForProvider(p)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("provider %q runtime resolver: %w", p.ID, err)
		}
		factories[p.ID] = factory
	}

	// Seed logical_agents from the catalog. Upsert — idempotent across
	// restarts; preserves created_at + any future operator-set fields.
	// See ADR 0003.
	if n, err := seedLogicalAgents(db, cat.Agents); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("seed logical_agents: %w", err)
	} else if n > 0 {
		log.Printf("store: seeded %d logical_agent row(s) from catalog", n)
	}
	bus := events.NewBus(events.BusOptions{Persister: db})

	evSink := &eventSinkAdapter{bus: bus}
	mgr := agentsessions.NewManager(stateSinkAdapter{db: db}).
		WithAttachmentSink(attachmentSinkAdapter{db: db}).
		WithEventSink(evSink)
	evSink.SetManager(mgr)

	brk := broker.NewService(db, bus)

	// Compose the v0.6 federation directory service. Storage attaches to
	// the same *sql.DB so registry rows live in state.db alongside
	// sessions/messages. file:// + cli:// resolvers are wired here so
	// Sync works without per-call resolver assembly; the file resolver
	// is anchored at the catalog root (D9 + symlink-escape defense).
	regStorage := registry.NewStorage(db.DB())
	regOpts := []registry.ServiceOption{registry.WithResolver(registry.NewCLIResolver())}
	if fr, ferr := registry.NewFileResolver(catalogRoot); ferr == nil {
		regOpts = append(regOpts, registry.WithResolver(fr))
	} else {
		log.Printf("registry: file resolver disabled — %v (sync over file:// will return ErrNoResolver)", ferr)
	}
	regSvc := registry.NewService(regStorage, regOpts...)

	// Compose the authority-routing federation Router over the local
	// messaging store. Returns nil when federation is disabled (the
	// standalone default) — the daemon then behaves exactly as before.
	fedRouter, err := federation.BuildRouter(
		cat.Global.Federation,
		db.MessagingStore(),
		federation.HTTPDialer(nil),
	)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("federation: %w", err)
	}
	if fedRouter != nil {
		log.Printf("federation: enabled — local authority %q, %d peer(s): %v",
			fedRouter.LocalAuthority(), len(fedRouter.Authorities()), fedRouter.Authorities())
	}

	return &Service{
		CatalogRoot: catalogRoot,
		Catalog:     cat,
		Store:       db,
		Manager:     mgr,
		Bus:         bus,
		Broker:      brk,
		Federation:  fedRouter,
		Registry:    regSvc,
		factories:   factories,
	}, nil
}

// ReconcileStaleState sweeps any sessions stuck in launching/running and
// any open client_attachments to terminal state. Intended for daemon
// startup only — `mux mcp` and other catalog-reading subcommands MUST
// NOT call this, because they may run concurrently with a live daemon
// (e.g. when a session spawns mux mcp as an MCP subprocess), and
// sweeping would clobber the daemon's actively-tracked sessions. See
// ADR 0030 §sweep-race for the original incident.
func (s *Service) ReconcileStaleState() {
	now := time.Now().UTC().Format(time.RFC3339)
	if swept, err := s.Store.SweepStaleSessions(now); err == nil && swept > 0 {
		log.Printf("store: swept %d stale session(s) to failed", swept)
	}
	if swept, err := s.Store.SweepStaleAttachments(now); err == nil && swept > 0 {
		log.Printf("store: swept %d stale client_attachments row(s)", swept)
	}
}

// Close shuts down the agentsessions.Manager and closes the store. Safe
// to call multiple times only via the underlying components' contracts;
// callers should treat Close as one-shot.
func (s *Service) Close() error {
	if err := s.Manager.Shutdown(context.Background()); err != nil {
		return err
	}
	return s.Store.Close()
}

// seedLogicalAgents upserts a logical_agents row for every catalog agent.
// Returns the number of rows touched. Idempotent: re-running refreshes
// name/role and updated_at while preserving created_at per the Upsert
// contract. See ADR 0003.
func seedLogicalAgents(db *store.Store, agents map[string]config.Agent) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var n int
	for _, a := range agents {
		la := agent.LogicalAgent{
			ID:   a.ID,
			Name: a.Name,
			Role: strings.Join(a.Roles, ", "),
		}
		if err := db.UpsertLogicalAgent(la, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
