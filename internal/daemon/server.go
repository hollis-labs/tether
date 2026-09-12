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
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	otelprop "github.com/hollis-labs/go-otel/propagation"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
)

// muxVersion labels daemon.started events. Bumped per release.
const muxVersion = "0.2.0"

// Config bundles the resolved daemon runtime parameters. Callers are
// expected to have already run path expansion (config.Expand) on
// ListenAddr and PIDFile.
type Config struct {
	ListenAddr      string
	PIDFile         string
	ShutdownTimeout time.Duration
}

// Server holds the wiring for one daemon process. Run blocks until ctx
// is canceled; Close is the cleanup hook invoked after the runtime
// manager drains (typically it closes the store).
type Server struct {
	Config  Config
	Manager *agentsessions.Manager
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
	// LogsDir is the directory containing muxd.log. When set, the
	// GET /api/logs/daemon endpoint reads from LogsDir/muxd.log.
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
	// mounted /messages/ subtree, so no separate mux.Handle entry is
	// needed here (only the api.Deps wiring below).
	DeliveryTrace api.DeliveryTraceStore
	// DeliveryRepair is optional; when set, POST /messages/{id}/redrive
	// is enabled (T09, messaging vNext) -- same /messages/ subtree, same
	// no-separate-mux-entry reasoning as DeliveryTrace.
	DeliveryRepair api.DeliveryTraceStore
	// Retention is optional; when set, GET /messages/retention/candidates
	// and POST /messages/{id}/purge are enabled (T09, messaging vNext) --
	// same /messages/ subtree, same no-separate-mux-entry reasoning as
	// DeliveryTrace.
	Retention api.RetentionStore
	// A2A is optional; when non-nil, its handler is mounted at "/a2a/"
	// (prefix stripped) -- the bounded, explicitly namespaced A2A
	// interoperability adapter (T10, messaging vNext). Held as a bare
	// http.Handler (constructed from internal/a2aadapter.NewAdapter at
	// composition time, see cmd/mux/daemon.go) rather than that
	// package's concrete type, so this package's import set doesn't grow
	// for what is, from here, just another optional mounted handler --
	// matching the deliberately minimal set of internal packages this
	// file otherwise depends on. Absent entirely means the A2A surface
	// doesn't exist on this daemon at all (T10 acceptance #3: "the
	// feature stays optional for local messaging").
	A2A   http.Handler
	Close func() error

	// WakeSweeper is optional; when set, Run starts a periodic background
	// pass (wakeSweepInterval) retrying wake attempts the delivery core
	// already knows are ready -- deliveries a prior wake attempt Nacked as
	// busy/offline (T06, messaging vNext). Populated from app.Service at
	// daemon startup; nil in tests that don't need the pump (e.g. Handler
	// tests that never call Run).
	WakeSweeper WakeSweeper

	startedAt time.Time
}

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

// runWakeSweepLoop ticks RunWakeSweep until ctx is canceled. Errors are
// logged, not fatal -- a sweep failure must never bring down the daemon;
// the next tick simply tries again.
func (s *Server) runWakeSweepLoop(ctx context.Context) {
	ticker := time.NewTicker(wakeSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.WakeSweeper.RunWakeSweep(ctx); err != nil {
				log.Printf("daemon: wake sweep failed: %v", err)
			}
		}
	}
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
	s.startedAt = time.Now()

	// Pre-flight: refuse to start if the PID file references a live process.
	// A stale PID file (dead PID or missing file) is fine and will be
	// overwritten by WritePIDFile below.
	if pid, err := ReadPIDFile(s.Config.PIDFile); err == nil && IsAlive(pid) {
		return fmt.Errorf("%w (pid %d, pidfile %s)", ErrAlreadyRunning, pid, s.Config.PIDFile)
	}

	lis, err := Listener(s.Config.ListenAddr)
	if err != nil {
		return fmt.Errorf("open listener %s: %w", s.Config.ListenAddr, err)
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
	}{muxVersion, os.Getpid(), s.Config.ListenAddr}); err == nil {
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

	var runErr error
	select {
	case <-ctx.Done():
		// Graceful shutdown path.
	case err := <-serveErr:
		// Unexpected listener death.
		runErr = err
	}

	s.publishDaemon(events.KindDaemonShutdownStarted, "")

	// Graceful shutdown: stop accepting connections, bound by timeout.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.Config.ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("http shutdown: %w", err)
	}

	// Drain runtime sessions (Manager.Shutdown waits for watch goroutines).
	if s.Manager != nil {
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
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	if s.A2A != nil {
		// A totally separate handler tree from api.NewHandler -- A2A has
		// its own wire format (JSON-RPC/well-known-card), not Tether's
		// writeJSON/CodeXxx envelope, so it is deliberately NOT routed
		// through apiHandler below (T10, messaging vNext).
		mux.Handle("/a2a/", http.StripPrefix("/a2a", s.A2A))
	}
	if s.Service != nil || s.Catalog != nil || s.AI != nil {
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
			DeliveryClaims:      s.DeliveryClaims,
			Attachments:         s.Attachments,
			ProxyEvents:         s.ProxyEvents,
			Registry:            s.Registry,
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
				mux.Handle(m.path, apiHandler)
			}
		}
	}
	return otelprop.HTTPMiddleware(mux)
}

// Health is the response body shape for GET /health. Kept small on purpose —
// full session/API surfaces arrive in Sprint v002-s05.
type Health struct {
	Status    string `json:"status"`
	PID       int    `json:"pid"`
	UptimeSec int64  `json:"uptime_sec"`
	Listener  string `json:"listener"`
	Sessions  int    `json:"sessions"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := Health{
		Status:    "ok",
		PID:       os.Getpid(),
		UptimeSec: int64(time.Since(s.startedAt).Seconds()),
		Listener:  s.Config.ListenAddr,
	}
	if s.Manager != nil {
		h.Sessions = len(s.Manager.List())
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

// apiMount is one entry in the outer mux's allowlist: a top-level path
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
// It used to be a run of mux.Handle calls inline in Handler(), which meant the
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

		{"/groups", s.Groups != nil},
		{"/groups/", s.Groups != nil},
		{"/mentions", s.Groups != nil},

		{"/logs/daemon", s.LogsDir != ""},
		{"/sessions/bootstrap", s.SessionBootstrap != nil},

		{"/fs/validate", true},
		{"/fs/detect", true},
	}
}
