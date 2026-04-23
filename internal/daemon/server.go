package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/events"
	"github.com/chrispian/agent-mux/internal/runtime"
)

// muxVersion labels daemon.started events. Bumped per release.
const muxVersion = "0.0.2"

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
	Manager *runtime.Manager
	// Service is the LaunchService the HTTP handlers dispatch to. When nil,
	// only /health is registered — useful for tests that don't need the
	// session surface.
	Service api.LaunchService
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
	// MessageStore is optional; when set, /messages/* endpoints are mounted.
	MessageStore api.MessageStore
	// Attachments is optional; when set, GET /sessions/{id}/attachments works.
	Attachments api.AttachmentStore
	// ProxyEvents is optional; when set, GET/POST /proxy/events endpoints are
	// mounted. Populated by the daemon when MCP proxy forwarding is active,
	// so the TUI can poll tool call events without sharing in-process memory
	// with the MCP subprocess.
	ProxyEvents api.ProxyEventStore
	// Publisher receives daemon.started / daemon.shutdown_started /
	// daemon.shutdown_completed events. Nil is a no-op.
	Publisher events.Publisher
	Close     func() error

	startedAt time.Time
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
	if s.Service != nil || s.Catalog != nil {
		apiHandler := api.NewHandler(api.Deps{
			Service:      s.Service,
			Checkpoints:  s.Checkpoints,
			Broker:       s.Broker,
			Bus:          s.Bus,
			EventsStore:  s.EventsStore,
			Catalog:      s.Catalog,
			GroupStore:   s.GroupStore,
			MessageStore: s.MessageStore,
			Attachments:  s.Attachments,
			ProxyEvents:  s.ProxyEvents,
		})
		// Mount api at every top-level path it owns. Keeping the list
		// explicit avoids a catch-all "/" that would shadow /health.
		if s.Service != nil {
			mux.Handle("/sessions", apiHandler)
			mux.Handle("/sessions/", apiHandler)
		}
		if s.Checkpoints != nil {
			mux.Handle("/logical-agents/", apiHandler)
		}
		if s.Broker != nil {
			mux.Handle("/broker/envelopes", apiHandler)
			mux.Handle("/broker/envelopes/", apiHandler)
			mux.Handle("/broker/requests", apiHandler)
		}
		if s.GroupStore != nil {
			mux.Handle("/session-groups", apiHandler)
			mux.Handle("/session-groups/", apiHandler)
		}
		if s.MessageStore != nil {
			mux.Handle("/messages", apiHandler)
			mux.Handle("/messages/", apiHandler)
			mux.Handle("/messages/subscribe", apiHandler)
			mux.Handle("/messages/inbox", apiHandler)
			mux.Handle("/messages/request", apiHandler)
		}
		if s.Bus != nil {
			mux.Handle("/events/stream", apiHandler)
		}
		if s.ProxyEvents != nil {
			mux.Handle("/proxy/events", apiHandler)
		}
		if s.Catalog != nil {
			mux.Handle("/catalog/projects", apiHandler)
			mux.Handle("/catalog/agents", apiHandler)
			mux.Handle("/catalog/providers", apiHandler)
			mux.Handle("/catalog/launches", apiHandler)
		}
	}
	return mux
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
