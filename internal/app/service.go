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

	"github.com/hollis-labs/go-agent-sessions/agentsessions"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/broker"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
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

	factories map[string]RuntimeFactory

	// codexThreads caches the JSON-RPC thread id per session id for
	// JsonRpcStdio runtimes. Populated lazily on the first SendTurn call
	// for a session (after initialize + thread/start succeed).
	codexThreads sync.Map // map[string]string
}

// New constructs a Service rooted at catalogRoot. Reads + validates the
// catalog, opens the SQLite store at the catalog's configured path, seeds
// logical_agents from catalog agents, wires the agentsessions.Manager
// with state/attachment/event sinks, and registers the built-in + catalog-
// declared runtime factories.
func New(catalogRoot string) (*Service, error) {
	cat, err := config.Load(catalogRoot)
	if err != nil {
		return nil, err
	}
	if err := cat.Validate(); err != nil {
		return nil, err
	}
	dbPath := config.Expand(cat.Global.Catalog.Defaults.StateDB)
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
	return &Service{
		CatalogRoot: catalogRoot,
		Catalog:     cat,
		Store:       db,
		Manager:     mgr,
		Bus:         bus,
		Broker:      brk,
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
