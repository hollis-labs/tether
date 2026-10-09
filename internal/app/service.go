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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/adapters/turn"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/app/turnrouting"
	"github.com/hollis-labs/tether/internal/broker"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/federation"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/settings"
	"github.com/hollis-labs/tether/internal/setup"
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
	// Interrupt bounds are configured before use; zero selects 2s cancel/gate
	// and 30s terminal defaults. Caller deadlines take precedence.
	InterruptCancelTimeout time.Duration
	InterruptDoneTimeout   time.Duration
	Channels               *channels.Service // constructed before starting any channel publisher
	turnOutputTimeout      time.Duration     // tests may shorten the default persistence deadline
	turnOutputStore        turnOutputStore
	turnRouter             *turnrouting.Router
	turnFeeds              map[string]turnFeedRegistration
	outputRetries          outputRetryState
	turnOutputs            sync.Map // session ID -> *sessionTurnOutput; runtime-owned completion state

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
	// composition contexts (e.g. read-only `tether mcp` connecting to a
	// remote daemon for session ops). MCP / HTTP / CLI surfaces that
	// depend on it must nil-guard before dispatching.
	Registry *registry.Service

	// Settings is the Global > Project > User settings cascade service
	// (CW-20260914-0042) for onboarding and deployment configuration.
	Settings *settings.Service

	launchMu    sync.Mutex
	reaperMu    sync.Mutex
	reaper      *sessionReaper
	resumeLocks sync.Map      // logical agent ID -> context-aware resume gate
	resumeGrace time.Duration // zero selects the bounded native fast-death window
	// RecoveryReadTool is a trusted daemon read port over its existing MCP
	// runtime and grants. Nil/unavailable inputs are explicit pack omissions.
	RecoveryReadTool func(context.Context, string, string, string, map[string]any) (json.RawMessage, error)
	BootTeamRecovery func(context.Context) error
	shimMu           sync.Mutex
	shimDraining     sync.Map
	shimBindingWait  sync.Map
	shimHosting      *shimHosting
	launches         map[string]*sessionLaunchGate

	factories map[string]RuntimeFactory

	// strictMCPStatus, when set, replaces the daemon-environment decision
	// ClaudeStrictMCPStatus takes. Tests use it.
	strictMCPStatus func() StrictMCPStatus

	// stops is shared with the Manager's state and event sinks; see
	// stopRequests. Nil when the Manager was built without
	// newSessionManager, in which case a stop is recorded as "completed".
	stops *stopRequests

	// codexThreads caches Codex app-server JSON-RPC thread state per
	// Tether session id. Populated lazily on the first SendTurn call.
	codexThreads turn.CodexAppServerCache

	// subprocessLogs holds each running subprocess-runtime session's
	// *subprocessLog, keyed by session id, from launch until the session
	// ends (CW-20261001-0033).
	subprocessLogs sync.Map

	// specResolver is the S5 Spec-path launch resolver. It is constructed
	// lazily on first use (only when the launch engine is "spec") via
	// specResolverOnce and reused across launches. specResolverErr records
	// a construction failure so it surfaces on every callsite, not just
	// the first. See launch_engine.go and specResolverFor.
	specResolverOnce sync.Once
	specResolver     *specresolve.Resolver
	specResolverErr  error

	// procs answers the daemon-start sweep's questions about a session's
	// recorded pid. nil means the OS (osProcessInspector); tests substitute.
	procs processInspector

	// wakePark carries the wake sweep's parked deliveries between ticks.
	// See wakeParkSet in wake.go.
	wakePark wakeParkSet

	// codexExempt holds, by session id, each running codex session that Tether
	// left to codex's own sandbox, so each turn can be re-checked
	// (refuseWidenedCodex). Set at launch, removed when the session ends.
	codexExempt sync.Map

	// protectionStatus decides whether launched agents get Tether's own
	// directories as ProtectedPaths; nil uses the daemon's OS and
	// environment. See protected_paths.go.
	protectionStatus func() ProtectionStatus
	// protectionWarned remembers which skipped project layers were already
	// warned about, so a launch does not repeat the warning every time.
	protectionWarned sync.Map

	// replies is the reply-to-sender dispatcher (CW-20261002-0065), set by
	// StartRoutingReplies. Nil means the reply path is not installed.
	replies atomic.Pointer[replyDispatcher]
	// interrupter backs interrupt:true on a reply; see routing_reply.go.
	interrupter turnInterrupter
	// ReplyAuthorization is the reply caller-identity hook; nil is observe mode.
	ReplyAuthorization ReplyAuthorization
}

// New constructs a Service rooted at catalogRoot. Reads + validates the
// catalog, opens the SQLite store at the catalog's configured path, seeds
// logical_agents from catalog agents, wires the agentsessions.Manager
// with state/attachment/event sinks, and registers the built-in + catalog-
// declared runtime factories.
func New(catalogRoot string) (*Service, error) {
	return newService(catalogRoot, false)
}

// NewDaemon validates every declared MCP grant at an operator's explicit
// startup. Ordinary CLI services remain usable for catalog inspection/repair.
func NewDaemon(catalogRoot string) (*Service, error) {
	return newService(catalogRoot, true)
}

func newService(catalogRoot string, validateMCPGrants bool) (*Service, error) {
	maybeAutoSeedCatalog(catalogRoot)

	cat, err := config.LoadLayered(catalogRoot)
	if err != nil {
		return nil, err
	}
	if err := cat.Validate(); err != nil {
		return nil, err
	}
	if validateMCPGrants {
		if err := cat.ValidateMCPGrants(); err != nil {
			return nil, err
		}
	}
	// An agent naming an undefined sandbox profile does not stop the daemon;
	// its launches are refused (CW-20261001-0130). Say so at startup.
	for _, issue := range cat.SandboxIssues() {
		log.Printf("catalog: %v; launches of this agent will be refused until the profile exists", issue)
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

	mgr, stops := newSessionManager(db, bus)

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

	setStorage := settings.NewStorage(db.DB())
	setSvc := settings.NewService(setStorage)

	service := &Service{
		CatalogRoot: catalogRoot,
		Catalog:     cat,
		Store:       db,
		Manager:     mgr,
		stops:       stops,
		Bus:         bus,
		Broker:      brk,
		Federation:  fedRouter,
		Registry:    regSvc,
		Settings:    setSvc,
		factories:   factories,
	}
	service.Channels = channels.New(db, nil)
	service.installTurnFeeds()
	if validateMCPGrants {
		if err := service.startTurnRouter(); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("turn router: %w", err)
		}
	}
	return service, nil
}

// NewCatalogOnly constructs a Service holding only the catalog at
// catalogRoot: no state database, event bus, session manager, broker or
// registry. It is the Service of a daemon-only `tether mcp`, which reads and
// writes Tether's state over the daemon API and must never open the database
// (CW-20261001-0173). Unlike New it never seeds a missing catalog: the
// catalog is the daemon's to write.
func NewCatalogOnly(catalogRoot string) (*Service, error) {
	cat, err := config.LoadLayered(catalogRoot)
	if err != nil {
		return nil, err
	}
	if err := cat.Validate(); err != nil {
		return nil, err
	}
	return &Service{CatalogRoot: catalogRoot, Catalog: cat}, nil
}

// newSessionManager wires an agentsessions.Manager to db and bus through
// the state, attachment and event sinks, sharing one stopRequests between
// the sinks and Service.StopSession so a stop is recorded as "killed".
func newSessionManager(db *store.Store, bus events.Publisher) (*agentsessions.Manager, *stopRequests) {
	stops := &stopRequests{}
	evSink := &eventSinkAdapter{bus: bus, stops: stops, db: db}
	mgr := agentsessions.NewManager(stateSinkAdapter{db: db, stops: stops}).
		WithAttachmentSink(attachmentSinkAdapter{db: db}).
		WithEventSink(evSink)
	evSink.SetManager(mgr)
	return mgr, stops
}

// ReconcileStaleState settles sessions the previous daemon left in
// launching/running/detached, and any open client_attachments. Detached
// sessions without shim reconciliation become orphaned, with authority revoked;
// existing orphaned rows are left alone. Intended for daemon
// startup only — `tether mcp` and other catalog-reading subcommands MUST
// NOT call this, because they may run concurrently with a live daemon
// (e.g. when a session spawns tether mcp as an MCP subprocess), and
// sweeping would clobber the daemon's actively-tracked sessions. See
// ADR 0030 §sweep-race for the original incident.
//
// A session whose process survived the restart is spared (CW-20260912-0085):
// its row keeps its state, though this daemon holds no runtime handle for it
// and cannot steer or stop it. Shim reconciliation handles detached sessions
// separately; direct-launch process survivors keep this legacy behavior.
// Every other one is failed with exit_code -1, "swept at daemon start", and
// its bindings are revoked below. On a systemd host with the default
// KillMode=control-group the agents die with the daemon, so a restart there
// sweeps them all; survivors happen when the daemon ran outside a unit that
// kills its children (`tether daemon start`, launchd).
func (s *Service) ReconcileStaleState() {
	now := time.Now().UTC().Format(time.RFC3339)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	swept, spared, err := s.Store.SweepStaleSessions(now, func(row store.StaleSession) bool { return s.sessionProcessSurvivedContext(ctx, row) })
	if err != nil {
		log.Printf("store: startup sweep failed: %v", err)
	}
	var failed []string
	for _, id := range swept {
		row, err := s.Store.GetSession(id)
		if err == nil && row.State == string(session.StateOrphaned) {
			s.publishSessionStateChange(store.SessionStateChange{SessionID: id, LogicalAgentID: row.LogicalAgentID, From: string(session.StateDetached), To: row.State, Reason: "shim_reconcile_disabled"})
		} else {
			failed = append(failed, id)
		}
	}
	s.reportSweep(failed, spared)
	if swept, err := s.Store.SweepStaleAttachments(now); err == nil && swept > 0 {
		log.Printf("store: swept %d stale client_attachments row(s)", swept)
	}
	if revoked := s.revokeEndedSessionBindings(context.Background()); revoked > 0 {
		log.Printf("registry: revoked %d binding(s) held by ended sessions", revoked)
	}
}

// sessionProcessSurvivedContext reports whether a stale session's own process is
// still alive. The pid must be alive and still be the process the session
// started: same start time as recorded at launch. A session launched before
// start times were recorded falls back to a weaker test: the process started
// no earlier than the session was created and runs the session's launch
// command. Anything that cannot be verified is not a survivor.
func (s *Service) sessionProcessSurvivedContext(ctx context.Context, row store.StaleSession) bool {
	if s.reconcileShimContext(ctx, row) {
		return true
	}
	if row.State == string(session.StateDetached) {
		return false
	}
	procs := s.procs
	if procs == nil {
		procs = osProcessInspector{}
	}
	if row.PID <= 0 || !procs.alive(row.PID) {
		return false
	}
	started, ok := procs.startTime(row.PID)
	if !ok {
		return false
	}
	if row.PIDStartedAt != "" {
		return started == row.PIDStartedAt
	}
	startedAt, err := time.Parse(time.RFC3339, started)
	if err != nil {
		return false
	}
	created, err := time.Parse(time.RFC3339, row.CreatedAt)
	if err != nil || startedAt.Before(created.Add(-2*time.Second)) {
		return false
	}
	plan, err := s.Store.GetLaunchPlan(row.ID)
	if err != nil || plan.Command == "" {
		return false
	}
	cmdline, ok := procs.command(row.PID)
	return ok && strings.Contains(cmdline, filepath.Base(plan.Command))
}

// reportSweep logs what the startup sweep did and publishes it as one
// daemon.sessions_swept event, so a swept session is traceable to the
// restart that swept it rather than only by timestamp correlation.
func (s *Service) reportSweep(swept, spared []string) {
	if len(swept) == 0 && len(spared) == 0 {
		return
	}
	if len(swept) > 0 {
		log.Printf("store: startup sweep failed %d session(s) whose process did not survive the restart (exit_code -1): %s", len(swept), strings.Join(swept, ", "))
	}
	if len(spared) > 0 {
		log.Printf("store: startup sweep left %d session(s) in place retained or handled by process recovery: %s", len(spared), strings.Join(spared, ", "))
	}
	if s.Bus == nil {
		return
	}
	payload, err := json.Marshal(sessionsSweptPayload{
		Swept:            len(swept),
		SweptSessionIDs:  nonNil(swept),
		Spared:           len(spared),
		SparedSessionIDs: nonNil(spared),
	})
	if err != nil {
		return
	}
	ctx, cancel := s.outputPersistenceContext()
	defer cancel()
	_ = s.Bus.Publish(ctx, events.Event{
		Scope:       events.ScopeDaemon,
		Kind:        events.KindDaemonSessionsSwept,
		PayloadJSON: string(payload),
	})
}

type sessionsSweptPayload struct {
	Swept            int      `json:"swept"`
	SweptSessionIDs  []string `json:"swept_session_ids"`
	Spared           int      `json:"spared"`
	SparedSessionIDs []string `json:"spared_session_ids"`
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// revokeEndedSessionBindings revokes the bindings of every Tether session
// that has ended -- including those the sweep above just failed -- so a
// restart does not leave an actor bound to a session that no longer exists
// (CW-20260912-0134). It also clears bindings orphaned before session exit
// revoked them. A binding whose session_id is not a Tether session (a
// published-local bridge) is left alone. Returns how many it revoked.
func (s *Service) revokeEndedSessionBindings(ctx context.Context) int {
	if s.Registry == nil {
		return 0
	}
	ids, err := s.Registry.BoundSessionIDs(ctx)
	if err != nil {
		log.Printf("registry: list bound sessions for the startup sweep failed: %v", err)
		return 0
	}
	revoked := 0
	for _, id := range ids {
		row, err := s.Store.GetSession(id)
		if err != nil {
			continue // not a Tether session, or unreadable: not ours to revoke
		}
		switch session.State(row.State) {
		case session.StateCompleted, session.StateFailed, session.StateKilled, session.StateOrphaned:
		default:
			continue
		}
		if s.retainedTeamRecoveryEligible(ctx, row) {
			continue
		}
		n, err := s.Registry.RevokeSessionBindings(ctx, id)
		if err != nil {
			log.Printf("registry: revoke bindings of ended session %q failed: %v", id, err)
			continue
		}
		revoked += n
	}
	return revoked
}

// Close shuts down the agentsessions.Manager and closes the store. Safe
// to call multiple times only via the underlying components' contracts;
// callers should treat Close as one-shot.
func (s *Service) Close() error {
	s.StopSessionReaper()
	if s.Manager != nil {
		if err := s.Manager.Shutdown(context.Background()); err != nil {
			return err
		}
	}
	s.stopRoutingReplies()
	// Complete output persistence before closing the store. Session watchers
	// also flush; the reducer lock makes this idempotent against those races.
	s.turnOutputs.Range(func(_, value any) bool {
		value.(*sessionTurnOutput).flush()
		return true
	})
	s.stopOutputRetries()
	s.turnRouter.Close()
	if s.Store == nil {
		return nil
	}
	return s.Store.Close()
}

// maybeAutoSeedCatalog writes a minimal starter catalog when the catalog root
// has no global.yaml. This is the non-interactive safety net (D3): the daemon
// never hard-fails on a missing catalog. Detection and full guided setup are
// the job of tether init — auto-seed only writes blank-command providers and the
// minimum needed for the daemon to start.
func maybeAutoSeedCatalog(catalogRoot string) {
	globalYAML := filepath.Join(config.Expand(catalogRoot), "global.yaml")
	if _, err := os.Stat(globalYAML); err == nil {
		return // catalog already present — no-op
	}
	stateRoot := filepath.Dir(config.Expand(catalogRoot))
	if _, err := setup.WriteCatalog(stateRoot, setup.WriteOpts{
		Minimal:   true,
		StateRoot: stateRoot,
	}); err != nil {
		log.Printf("auto-seed: failed to seed minimal catalog at %s: %v", stateRoot, err)
		return
	}
	log.Printf("catalog absent — seeded minimal catalog at %s; run 'tether init' for guided setup", stateRoot)
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
