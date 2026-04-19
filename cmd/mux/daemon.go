package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/app"
	"github.com/chrispian/agent-mux/internal/client"
	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/daemon"
	"github.com/chrispian/agent-mux/internal/store"
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

		child := exec.Command(os.Args[0], "daemon", "run", "--catalog", catalogPath)
		child.Stdout = nil
		child.Stderr = nil
		child.Stdin = nil
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			return fmt.Errorf("spawn daemon: %w", err)
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
		svc, err := app.New(catalogPath)
		if err != nil {
			return err
		}

		cfg, err := daemonConfigFromCatalog(svc.Catalog)
		if err != nil {
			_ = svc.Store.Close()
			return err
		}

		server := &daemon.Server{
			Config:    cfg,
			Manager:   svc.Runtime,
			Service:   &serviceAdapter{svc: svc},
			Publisher: svc.Bus,
			Close: func() error {
				// Manager.Shutdown is driven by daemon.Server; Close just
				// releases the store handle so the process can exit cleanly.
				return svc.Store.Close()
			},
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		return server.Run(ctx)
	},
}

// serviceAdapter bridges *app.Service to api.LaunchService. The only
// translation step is flattening app.Launched's *workspace.Session pointer
// into primitive strings so the API response never carries internal types.
type serviceAdapter struct {
	svc *app.Service
}

func (a *serviceAdapter) Launch(launchID string) (api.LaunchResult, error) {
	l, err := a.svc.Launch(launchID)
	if err != nil {
		return api.LaunchResult{}, err
	}
	return api.LaunchResult{
		SessionID: l.SessionID,
		Workspace: l.Workspace.Root,
		LogPath:   l.Workspace.LogPath,
	}, nil
}

func (a *serviceAdapter) ListSessions() ([]store.SessionRow, error) {
	return a.svc.ListSessions()
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

func (a *serviceAdapter) AttachSession(ctx context.Context, id string, w io.Writer) error {
	return a.svc.AttachSession(ctx, id, w)
}

func (a *serviceAdapter) AttachedClients(id string) int {
	return a.svc.AttachedClients(id)
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
	dbPath := config.Expand(cat.Global.Catalog.Defaults.StateDB)
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
