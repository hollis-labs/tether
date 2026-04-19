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

	"github.com/chrispian/agent-mux/internal/runtime"
)

// Config bundles the resolved daemon runtime parameters. Callers are
// expected to have already run path expansion (config.Expand) on
// ListenAddr and PIDFile.
type Config struct {
	ListenAddr      string
	PIDFile         string
	ShutdownTimeout time.Duration
}

// Server holds the wiring for one daemon process. Run blocks until ctx
// is cancelled; Close is the cleanup hook invoked after the runtime
// manager drains (typically it closes the store).
type Server struct {
	Config   Config
	Manager  *runtime.Manager
	Close    func() error

	startedAt time.Time
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

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
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
