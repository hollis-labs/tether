package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	otelprop "github.com/hollis-labs/go-otel/propagation"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
)

// tetherVersion labels daemon.started events. Bumped per release.
const tetherVersion = "0.2.0"

// Config bundles the resolved daemon runtime parameters. Callers are
// expected to have already run path expansion (config.Expand) on
// ListenAddr and PIDFile.
type Config struct {
	IdentityMode    identity.Mode
	ListenAddr      string
	PIDFile         string
	ShutdownTimeout time.Duration
}

// Server holds the wiring for one daemon process. Run blocks until ctx
// is canceled; Close is the cleanup hook invoked after the runtime
// manager drains (typically it closes the store).
type Server struct {
	Identity                 *identity.Store
	OperatorIdentityDegraded bool
	identityAuditMu          sync.Mutex
	identityAudit            *identity.AuditQueue
	Config                   Config
	Manager                  *agentsessions.Manager
	// SandboxProtect, when set, fills /health's sandbox_protect field. It
	// runs on each /health request, so it must be cheap or cache.
	SandboxProtect func() *SandboxProtectHealth
	// Service is the LaunchService the HTTP handlers dispatch to. When nil,
	// only /health is registered — useful for tests that don't need the
	// session surface.
	Service api.LaunchService
	// AI is optional; when set, /ai/chat is mounted.
	AI api.AIService
	// AIAudit is optional; when set, /ai/audit is mounted.
	AIAudit api.AIAuditStore
	// AIUsage is optional; when set, /ai/usage is mounted.
	AIUsage api.AIUsageStore
	// Checkpoints is optional; when set, the checkpoint endpoints are
	// mounted. Tests can pass nil to skip them.
	Checkpoints api.CheckpointStore
	// Broker is optional; when set, the broker envelope endpoints are
	// mounted.
	Broker api.BrokerService
	// Bus is optional; when set, /events/stream is mounted.
	Bus events.Bus
	// EventsStore is optional; when set, GET /sessions/{id}/events works.
	EventsStore api.EventsStore
	// Catalog is optional; when set, GET /catalog/<type> endpoints are
	// mounted. Nil in tests that only exercise the session surface.
	Catalog api.CatalogLoader
	// GroupStore is optional; when set, /session-groups endpoints are mounted.
	GroupStore api.SessionGroupStore
	// Workstreams is optional; when set, /workstreams endpoints and the
	// per-session workstream sub-resource are mounted (S1, CW-20260912-0059).
	Workstreams api.WorkstreamStore
	// SessionRefs is optional; when set, session-ref endpoints are mounted.
	SessionRefs api.SessionRefStore
	// Digests is optional; when set, the digest endpoints are mounted.
	Digests api.DigestStore
	// MessageStore is optional; when set, /messages/* endpoints are mounted.
	MessageStore api.MessageStore
	Channels     api.ChannelService
	Routing      api.RoutingService
	// DeliveryClaims is optional; when set, POST /messages/{id}/claim|ack|nack
	// are enabled (T07, messaging vNext) -- durable claim/ack/nack for a
	// caller pulling its own mailbox on its own initiative. Populated from
	// app.Service's underlying *store.Store.
	DeliveryClaims api.DeliveryClaimer
	// Attachments is optional; when set, GET /sessions/{id}/attachments works.
	Attachments api.AttachmentStore
	// Registry is optional; when set, /registry/* federation directory
	// endpoints are mounted. Populated by app.Service.Registry at daemon
	// startup.
	Registry api.RegistryService
	// Settings is optional; when set, /settings/* onboarding cascade endpoints
	// are mounted (CW-20260914-0042).
	Settings api.SettingsService
	// RegistryCatalogRoot is the catalog root the registry bootstrap
	// importer scans for `POST /registry/bootstrap`. Forwarded into
	// api.Deps; empty disables the endpoint.
	RegistryCatalogRoot string
	// Groups is optional; when set, /groups/* + /mentions v060-05
	// group-messaging endpoints are mounted. In production this is the
	// same *registry.Service instance used by Registry — the
	// GroupsService interface is the narrow seam over the group-specific
	// methods.
	Groups api.GroupsService
	// ProxyEvents is optional; when set, GET/POST /proxy/events endpoints are
	// mounted. Populated by the daemon when MCP proxy forwarding is active,
	// so the TUI can poll tool call events without sharing in-process memory
	// with the MCP subprocess.
	ProxyEvents api.ProxyEventStore
	// Publisher receives daemon.started / daemon.shutdown_started /
	// daemon.shutdown_completed events. Nil is a no-op.
	Publisher events.Publisher
	// LogsDir is the directory containing tetherd.log. When set, the
	// GET /api/logs/daemon endpoint reads from LogsDir/tetherd.log.
	// Empty disables the endpoint (returns 404).
	LogsDir string
	// SessionBootstrap is optional; when set, POST /sessions/bootstrap is
	// mounted (T08, messaging vNext) -- the provider-neutral local
	// bootstrap/registration helper an external launcher invokes at the
	// launch/host boundary. Populated from app.Service's underlying
	// *store.Store at daemon startup.
	SessionBootstrap api.SessionBootstrapStore
	// DeliveryTrace is optional; when set, GET /messages/{id}/trace is
	// enabled (T09, messaging vNext) -- served through the already-
	// mounted /messages/ subtree, so no separate router.Handle entry is
	// needed here (only the api.Deps wiring below).
	DeliveryTrace api.DeliveryTraceStore
	// DeliveryRepair is optional; when set, POST /messages/{id}/redrive
	// is enabled (T09, messaging vNext) -- same /messages/ subtree, same
	// no-separate-router-entry reasoning as DeliveryTrace.
	DeliveryRepair api.DeliveryTraceStore
	// Retention is optional; when set, GET /messages/retention/candidates
	// and POST /messages/{id}/purge are enabled (T09, messaging vNext) --
	// same /messages/ subtree, same no-separate-router-entry reasoning as
	// DeliveryTrace.
	Retention api.RetentionStore
	// A2A is optional; when non-nil, its handler is mounted at "/a2a/"
	// (prefix stripped) -- the bounded, explicitly namespaced A2A
	// interoperability adapter (T10, messaging vNext). Held as a bare
	// http.Handler (constructed from internal/a2aadapter.NewAdapter at
	// composition time, see cmd/router/daemon.go) rather than that
	// package's concrete type, so this package's import set doesn't grow
	// for what is, from here, just another optional mounted handler --
	// matching the deliberately minimal set of internal packages this
	// file otherwise depends on. Absent entirely means the A2A surface
	// doesn't exist on this daemon at all (T10 acceptance #3: "the
	// feature stays optional for local messaging").
	A2A   http.Handler
	Close func() error
	// MCP is the injected daemon-owned Streamable HTTP/UDS gateway. Its
	// lifecycle closes sessions/streams and upstreams before HTTP/store drain.
	MCP         http.Handler
	MCPShutdown func()
	MCPDrain    func(context.Context)

	// WakeSweeper is optional; when set, Run starts a periodic background
	// pass (wakeSweepInterval) retrying wake attempts the delivery core
	// already knows are ready -- deliveries a prior wake attempt Nacked as
	// busy/offline (T06, messaging vNext). Populated from app.Service at
	// daemon startup; nil in tests that don't need the pump (e.g. Handler
	// tests that never call Run).
	WakeSweeper WakeSweeper

	// SessionDrainer, when set, replaces the bare Manager.Shutdown drain on
	// a graceful shutdown. It records that the sessions it waits on ended
	// because the daemon was stopped on purpose, not by a crash
	// (CW-20260912-0086). *app.Service satisfies it (DrainSessions).
	SessionDrainer SessionDrainer

	// EventRetention is optional; when set, Run starts a periodic background
	// pass (eventRetentionInterval) deleting events older than the
	// configured retention window (CW-20260930-0008). The pass itself is a
	// no-op while retention is disabled in the catalog. Populated from
	// app.Service at daemon startup.
	EventRetention EventRetention

	// Hardening is optional; when set, /health reports what it returns. The
	// composition root points it at the Service's launch-hardening status.
	Hardening func() *HealthHardening

	startedAt time.Time
}

// SessionDrainer is the seam Run uses to drain sessions on a graceful
// shutdown. DrainSessions must return once every session has ended or ctx
// is done, as Manager.Shutdown does.
type SessionDrainer interface {
	DrainSessions(ctx context.Context) error
}

// EventRetention is the narrow seam Run uses to drive the events retention
// sweep. *app.Service satisfies it (RunEventRetention,
// internal/app/events_retention.go).
type EventRetention interface {
	RunEventRetention(ctx context.Context) (int64, error)
}

// eventRetentionInterval is how often the events retention sweep runs. The
// window is measured in days, so hourly is plenty. A var so tests can
// shrink it.
var eventRetentionInterval = time.Hour

// WakeSweeper is the narrow seam Run uses to drive the shared wake pump.
// *app.Service satisfies it directly (RunWakeSweep, internal/app/wake.go).
// A dedicated interface here (rather than reusing api.LaunchService) keeps
// this daemon-lifecycle concern independent of the HTTP-transport
// interface and its many existing test doubles.
type WakeSweeper interface {
	RunWakeSweep(ctx context.Context) (int, error)
}

// wakeSweepInterval bounds how often the background pump retries ready
// deliveries. Short enough that a busy session's queued wake is retried
// promptly once it goes idle; long enough not to hammer an offline actor.
// A var (not a const) solely so tests can shrink it instead of waiting out
// the real production interval.
var wakeSweepInterval = 5 * time.Second

// runPeriodic calls run every interval until ctx is canceled. Errors are
// logged, not fatal -- a periodic job failing must never bring down the
// daemon; the next tick simply tries again. It is the daemon's one
// periodic-job loop, shared by the wake sweep and events retention.
func runPeriodic(ctx context.Context, interval time.Duration, name string, run func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("daemon: %s failed: %v", name, err)
			}
		}
	}
}

// runWakeSweepLoop ticks RunWakeSweep until ctx is canceled.
func (s *Server) runWakeSweepLoop(ctx context.Context) {
	runPeriodic(ctx, wakeSweepInterval, "wake sweep", func(ctx context.Context) error {
		_, err := s.WakeSweeper.RunWakeSweep(ctx)
		return err
	})
}

// runEventRetentionLoop ticks RunEventRetention until ctx is canceled. The
// first pass runs one interval after startup, not during it.
func (s *Server) runEventRetentionLoop(ctx context.Context) {
	runPeriodic(ctx, eventRetentionInterval, "events retention", func(ctx context.Context) error {
		_, err := s.EventRetention.RunEventRetention(ctx)
		return err
	})
}

func (s *Server) publishDaemon(kind, payloadJSON string) {
	if s.Publisher == nil {
		return
	}
	_ = s.Publisher.Publish(context.Background(), events.Event{
		Scope:       events.ScopeDaemon,
		Kind:        kind,
		PayloadJSON: payloadJSON,
	})
}

// Run starts the daemon: PID-file guard, listener open, HTTP serve,
// graceful shutdown on ctx cancellation. If another daemon is already
// running, Run returns ErrAlreadyRunning without having touched any
// state. Listener errors (e.g., socket already bound) likewise abort
// before the PID file is written.
func (s *Server) Run(ctx context.Context) error {
	if s.MCPShutdown != nil {
		defer s.MCPShutdown()
	}
	if err := identity.ValidateBind(s.Config.ListenAddr, s.Config.IdentityMode); err != nil {
		return err
	}
	defer s.CloseIdentityAudit()
	s.startedAt = time.Now()

	// Pre-flight: refuse to start if the PID file references a live process.
	// A stale PID file (dead PID or missing file) is fine and will be
	// overwritten by WritePIDFile below.
	if pid, err := ReadPIDFile(s.Config.PIDFile); err == nil && IsDaemonAlive(pid) {
		return fmt.Errorf("%w (pid %d, pidfile %s)", ErrAlreadyRunning, pid, s.Config.PIDFile)
	}

	lis, err := Listener(s.Config.ListenAddr)
	if err != nil {
		return fmt.Errorf("open listener %s: %w", s.Config.ListenAddr, err)
	}
	if s.MCP != nil {
		if err := validateMCPListenerAddress(lis.Addr(), s.Config.IdentityMode, s.Identity != nil); err != nil {
			_ = lis.Close()
			return err
		}
	}

	if err := WritePIDFile(s.Config.PIDFile, os.Getpid()); err != nil {
		lis.Close()
		return fmt.Errorf("write pidfile: %w", err)
	}

	httpSrv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Daemon is up; announce it. Errors are ignored (persister failure
	// must not gate serving).
	if b, err := json.Marshal(struct {
		Version  string `json:"version"`
		PID      int    `json:"pid"`
		Listener string `json:"listener"`
	}{tetherVersion, os.Getpid(), s.Config.ListenAddr}); err == nil {
		s.publishDaemon(events.KindDaemonStarted, string(b))
	}

	serveErr := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(lis)
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	if s.WakeSweeper != nil {
		go s.runWakeSweepLoop(ctx)
	}
	if s.EventRetention != nil {
		go s.runEventRetentionLoop(ctx)
	}

	var runErr error
	select {
	case <-ctx.Done():
		// Graceful shutdown path.
	case err := <-serveErr:
		// Unexpected listener death.
		runErr = err
	}

	s.publishDaemon(events.KindDaemonShutdownStarted, "")
	// Stop admission/streams, bounded drain, then close the pool before store.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.Config.ShutdownTimeout)
	defer cancel()
	if s.MCPDrain != nil {
		s.MCPDrain(shutdownCtx)
	}
	if s.MCPShutdown != nil {
		s.MCPShutdown()
	}

	// Graceful shutdown: stop accepting connections, using the same deadline.
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("http shutdown: %w", err)
	}

	// Drain runtime sessions (Manager.Shutdown waits for watch goroutines).
	// A SessionDrainer also records why they are ending.
	switch {
	case s.SessionDrainer != nil:
		if err := s.SessionDrainer.DrainSessions(shutdownCtx); err != nil && runErr == nil {
			runErr = fmt.Errorf("runtime shutdown: %w", err)
		}
	case s.Manager != nil:
		if err := s.Manager.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = fmt.Errorf("runtime shutdown: %w", err)
		}
	}

	// Emit shutdown_completed BEFORE Close(). Close typically tears down
	// the store, which is the bus's persister — publishing after Close
	// would silently fail to persist.
	s.publishDaemon(events.KindDaemonShutdownCompleted, "")

	if s.Close != nil {
		if err := s.Close(); err != nil && runErr == nil {
			runErr = fmt.Errorf("close: %w", err)
		}
	}

	if err := RemovePIDFile(s.Config.PIDFile); err != nil && runErr == nil {
		runErr = fmt.Errorf("remove pidfile: %w", err)
	}

	// Clean up socket file for unix transport; other modes are stateless.
	if sock := SocketPath(s.Config.ListenAddr); sock != "" {
		_ = os.Remove(sock)
	}

	return runErr
}

// Handler builds the http.Handler exposed by Run. Exposed so tests can
// exercise the routing table without spinning up a listener + PID file.
// /health is always registered; api routes are delegated to the api
// package when Service is non-nil.
func (s *Server) Handler() http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("/health", s.handleHealth)
	if s.MCP != nil {
		router.Handle("/mcp", s.MCP)
		router.Handle("/p/", s.MCP)
	}
	if s.A2A != nil {
		// A totally separate handler tree from api.NewHandler -- A2A has
		// its own wire format (JSON-RPC/well-known-card), not Tether's
		// writeJSON/CodeXxx envelope, so it is deliberately NOT routed
		// through apiHandler below (T10, messaging vNext).
		router.Handle("/a2a/", http.StripPrefix("/a2a", s.A2A))
	}
	if s.Service != nil || s.Catalog != nil || s.AI != nil || s.Channels != nil || s.Routing != nil {
		apiHandler := api.NewHandler(api.Deps{
			Service:             s.Service,
			AI:                  s.AI,
			AIAudit:             s.AIAudit,
			AIUsage:             s.AIUsage,
			Checkpoints:         s.Checkpoints,
			Broker:              s.Broker,
			Bus:                 s.Bus,
			EventsStore:         s.EventsStore,
			Catalog:             s.Catalog,
			GroupStore:          s.GroupStore,
			Workstreams:         s.Workstreams,
			SessionRefs:         s.SessionRefs,
			Digests:             s.Digests,
			MessageStore:        s.MessageStore,
			Channels:            s.Channels,
			Routing:             s.Routing,
			DeliveryClaims:      s.DeliveryClaims,
			Attachments:         s.Attachments,
			ProxyEvents:         s.ProxyEvents,
			Registry:            s.Registry,
			Settings:            s.Settings,
			RegistryCatalogRoot: s.RegistryCatalogRoot,
			Groups:              s.Groups,
			LogsDir:             s.LogsDir,
			SessionBootstrap:    s.SessionBootstrap,
			DeliveryTrace:       s.DeliveryTrace,
			DeliveryRepair:      s.DeliveryRepair,
			Retention:           s.Retention,
		})
		// Mount api at every top-level path it owns. Keeping the list
		// explicit avoids a catch-all "/" that would shadow /health.
		for _, m := range s.apiMounts() {
			if m.enabled {
				router.Handle(m.path, apiHandler)
			}
		}
	}
	return otelprop.HTTPMiddleware(identity.Middleware(s.Config.IdentityMode, s.Identity, s.recordIdentityObservation, router))
}

// Health is the response body shape for GET /health. Kept small on purpose —
// full session/API surfaces arrive in Sprint v002-s05.
type Health struct {
	Identity  *IdentityHealth `json:"identity,omitempty"`
	Status    string          `json:"status"`
	PID       int             `json:"pid"`
	UptimeSec int64           `json:"uptime_sec"`
	Listener  string          `json:"listener"`
	Sessions  int             `json:"sessions"`
	// Hardening reports the launch-hardening switches the running daemon
	// decided from its own environment, so `tether doctor` can report what
	// tetherd does rather than what the doctor's environment would do. Absent
	// from a daemon that predates it.
	Hardening *HealthHardening `json:"hardening,omitempty"`

	// SandboxProtect is the daemon's own view of control-plane protection
	// (CW-20261001-0142): decided from the daemon's environment, which `tether
	// doctor` cannot see from its shell. Absent from an older daemon.
	SandboxProtect *SandboxProtectHealth `json:"sandbox_protect,omitempty"`
}

// HealthHardening is the launch-hardening state in GET /health.
type HealthHardening struct {
	MCPUpstreamSessions map[string]int    `json:"mcp_upstream_sessions,omitempty"`
	MCPRecorder         map[string]uint64 `json:"mcp_recorder,omitempty"`
	// ClaudeStrictMCP is whether Claude agents load only the MCP servers
	// Tether plants (--strict-mcp-config, CW-20261001-0227).
	ClaudeStrictMCP       bool   `json:"claude_strict_mcp"`
	ClaudeStrictMCPReason string `json:"claude_strict_mcp_reason,omitempty"`
}

// SandboxProtectHealth reports whether the agents the daemon launches get
// Tether's catalog, run and state directories as read-only protected paths, and
// whether this host can provide that.
type SandboxProtectHealth struct {
	// Enabled is true when protection applies to launches.
	Enabled bool `json:"enabled"`
	// DisabledByOperator is true when TETHER_SANDBOX_PROTECT turned it off.
	DisabledByOperator bool `json:"disabled_by_operator,omitempty"`
	// Reason says, in a sentence, what the state means for an agent.
	Reason string `json:"reason"`
	// Codex is how codex is protected: "guarded", "not protected" (the
	// fallback) or "not applicable" (protection is off). CodexReason says what
	// that means, and names CW-20261001-0230, the structural reason (codex
	// spawns MCP servers outside its sandbox).
	Codex       string `json:"codex,omitempty"`
	CodexReason string `json:"codex_reason,omitempty"`
	// BwrapChecked is true when the host was probed (protection on, Linux).
	BwrapChecked bool `json:"bwrap_checked,omitempty"`
	// BwrapUsable is true when bubblewrap can build the protecting sandbox.
	BwrapUsable bool `json:"bwrap_usable,omitempty"`
	// BwrapError is why it cannot, when it cannot.
	BwrapError string `json:"bwrap_error,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := Health{
		Status:    "ok",
		PID:       os.Getpid(),
		UptimeSec: int64(time.Since(s.startedAt).Seconds()),
		Listener:  s.Config.ListenAddr,
	}
	h.Identity = s.identityHealth()
	if s.Manager != nil {
		h.Sessions = len(s.Manager.List())
	}
	if s.Hardening != nil {
		h.Hardening = s.Hardening()
	}
	if s.SandboxProtect != nil {
		h.SandboxProtect = s.SandboxProtect()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h)
}

// DialHTTPClient returns an *http.Client configured to speak to the daemon
// at addr. For unix: addresses, it dials the socket; for tcp: addresses,
// it uses a normal DialContext. Callers should use "http://unix/<path>"
// style URLs for unix transports — the host part is ignored.
func DialHTTPClient(addr string) *http.Client {
	if strings.HasPrefix(addr, "unix:") {
		sock := strings.TrimPrefix(addr, "unix:")
		return &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
			Timeout: 5 * time.Second,
		}
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// BaseURL returns a URL prefix suitable for http.Get against the daemon.
// For unix transports we use the conventional "http://unix" placeholder.
func BaseURL(addr string) string {
	if strings.HasPrefix(addr, "unix:") {
		return "http://unix"
	}
	if strings.HasPrefix(addr, "tcp:") {
		return "http://" + strings.TrimPrefix(addr, "tcp:")
	}
	return ""
}

// apiMount is one entry in the outer router's allowlist: a top-level path
// internal/api owns, and whether this Server has the dependency that makes it
// serviceable.
type apiMount struct {
	path    string
	enabled bool
}

// apiMounts is THE list of api paths the daemon routes, and the single source
// for both Handler() and the regression test that checks it against what
// internal/api actually registers.
//
// It used to be a run of router.Handle calls inline in Handler(), which meant the
// only way to check it was to make an HTTP request per path with every
// optional dependency stubbed. That is why CW-20260912-0059 shipped with
// /workstreams registered in api, wired in Deps, and never routed here: the
// feature was correct everywhere it was tested, and unreachable in production.
//
// ADDING A ROUTE TO internal/api MEANS ADDING IT HERE. Nothing derives one
// from the other -- Go's ServeMux exposes no way to enumerate its patterns --
// so route_mount_test.go compares this list against a maintained inventory of
// api's paths and fails when they diverge.
func (s *Server) apiMounts() []apiMount {
	hasService := s.Service != nil
	hasAI := s.AI != nil
	return []apiMount{
		{"/sessions", hasService},
		{"/sessions/", hasService},

		// Both forms, and the bare one is NOT redundant: api registers
		// /logical-agents and /logical-agents/ as two different handlers
		// (collection vs item, checkpoints.go:128-129). Mounting only the
		// subtree left the collection unreachable — ServeMux redirected the
		// bare path into the subtree, so a list request silently arrived at
		// the item handler with an empty id. Found by the CW-20260912-0059
		// audit, same class and cause as /workstreams.
		{"/logical-agents", hasService},
		{"/logical-agents/", hasService},

		{"/ai/chat", hasAI},
		{"/ai/chat/stream", hasAI},
		{"/ai/embeddings", hasAI},
		{"/ai/providers", hasAI},
		{"/ai/models", hasAI},
		{"/ai/routes", hasAI},
		{"/ai/routes/explain", hasAI},
		{"/ai/routes/preview", hasAI},
		{"/ai/usage", hasAI && s.AIUsage != nil},
		{"/ai/budgets", hasAI && s.AIUsage != nil},
		{"/ai/audit", hasAI && s.AIAudit != nil},

		{"/broker/envelopes", s.Broker != nil},
		{"/broker/envelopes/", s.Broker != nil},
		{"/broker/requests", s.Broker != nil},

		{"/session-groups", s.GroupStore != nil},
		{"/session-groups/", s.GroupStore != nil},

		// Both forms: "/workstreams" for the collection and "/workstreams/"
		// as the subtree covering /{id} and /{id}/refs. Registering only the
		// subtree would leave the collection reachable solely via ServeMux's
		// redirect.
		{"/workstreams", s.Workstreams != nil},
		{"/workstreams/", s.Workstreams != nil},

		// "/messages/" is a subtree pattern; the explicit siblings below it
		// are listed because api registers them as exact patterns, and an
		// exact pattern must be mounted to take precedence over the subtree.
		{"/routing/capabilities", s.Routing != nil},
		{"/channels", s.Channels != nil},
		{"/channels/", s.Channels != nil},
		{"/messages", s.MessageStore != nil},
		{"/messages/", s.MessageStore != nil},
		{"/messages/subscribe", s.MessageStore != nil},
		{"/messages/inbox", s.MessageStore != nil},
		{"/messages/list", s.MessageStore != nil},
		{"/messages/notify", s.MessageStore != nil},
		{"/messages/request", s.MessageStore != nil},
		{"/messages/thread/", s.MessageStore != nil},
		{"/messages/retention/candidates", s.MessageStore != nil},

		{"/events", s.EventsStore != nil},
		{"/events/stream", s.Bus != nil},
		{"/proxy/events", s.ProxyEvents != nil},

		{"/catalog/projects", s.Catalog != nil},
		{"/catalog/agents", s.Catalog != nil},
		{"/catalog/providers", s.Catalog != nil},
		{"/catalog/launches", s.Catalog != nil},

		// "/registry/" is a subtree pattern -- it already covers
		// /registry/bindings* and /registry/scoped-bindings* (T07, T08)
		// without a separate entry per subpath.
		{"/registry/", s.Registry != nil},
		{"/whoami", s.Registry != nil},

		{"/settings/mcp", s.Settings != nil},
		{"/auth/context", true},
		{"/settings/onboarding", s.Settings != nil},
		{"/settings/onboarding/", s.Settings != nil},

		{"/groups", s.Groups != nil},
		{"/groups/", s.Groups != nil},
		{"/mentions", s.Groups != nil},

		{"/logs/daemon", s.LogsDir != ""},
		{"/sessions/bootstrap", s.SessionBootstrap != nil},

		{"/fs/validate", true},
		{"/fs/detect", true},
	}
}
