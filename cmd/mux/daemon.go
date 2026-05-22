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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/broker"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/messaging"
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

		// Bootstrap the federation directory from ~/.tether/catalog/. Runs
		// after migrations (already applied via store.Open in app.New) and
		// BEFORE the HTTP listener binds (server.Run, below) so the daemon
		// publishes a fully-populated /registry surface on startup-complete.
		// Non-force: existing rows (matched by callback.target) are left
		// alone; only new files import. Operators apply catalog drift via
		// `mux registry bootstrap --force`.
		if svc.Registry != nil && svc.CatalogRoot != "" {
			report, err := svc.Registry.BootstrapFromCatalog(ctx, svc.CatalogRoot, false)
			if err != nil {
				log.Printf("registry bootstrap: %v", err)
			} else {
				log.Printf("registry bootstrap: imported=%d skipped=%d refreshed=%d errors=%d",
					report.Imported, report.Skipped, report.Refreshed, len(report.Errors))
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

		server := &daemon.Server{
			Config:              cfg,
			Manager:             svc.Manager,
			Service:             &serviceAdapter{svc: svc},
			Checkpoints:         svc.Store,
			Broker:              &brokerAdapter{write: svc.Broker, read: svc.Store},
			Bus:                 svc.Bus,
			EventsStore:         svc.Store,
			Catalog:             &catalogLoader{root: svc.CatalogRoot},
			GroupStore:          svc.Store,
			MessageStore:        svc.Store.MessagingStore(),
			Attachments:         svc.Store,
			ProxyEvents:         svc.Store,
			Registry:            svc.Registry,
			RegistryCatalogRoot: svc.CatalogRoot,
			Groups:              svc.Registry,
			Publisher:           svc.Bus,
			Close: func() error {
				// Manager.Shutdown is driven by daemon.Server; Close just
				// releases the store handle so the process can exit cleanly.
				return svc.Store.Close()
			},
		}

		return server.Run(ctx)
	},
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

func (a *serviceAdapter) RuntimeHealth(id string) (api.RuntimeHealthResult, bool) {
	return a.svc.RuntimeHealth(id)
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
