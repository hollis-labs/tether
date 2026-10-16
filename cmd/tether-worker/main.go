package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/hollis-labs/libs/harness/core/shim"
	"github.com/hollis-labs/tether/internal/outpostmail"
	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/hollis-labs/tether/internal/workerwire"
	"github.com/spf13/cobra"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".tether-worker")
	cmd := &cobra.Command{Use: "tether-worker", Short: "Thin execution outpost for the Tether hub"}
	cmd.PersistentFlags().StringVar(&root, "root", root, "worker state root")
	cmd.AddCommand(mailCommand(&root))
	var descriptor string
	shimCmd := &cobra.Command{Use: "shim-host", Hidden: true, RunE: func(c *cobra.Command, _ []string) error {
		spec, err := shim.ReadLaunch(descriptor)
		if err != nil {
			return err
		}
		host, err := shim.Start(spec)
		if err != nil {
			return err
		}
		<-c.Context().Done()
		return host.Close()
	}}
	shimCmd.Flags().StringVar(&descriptor, "launch", "", "private shim launch descriptor")
	cmd.AddCommand(shimCmd)
	var relayToken string
	relayCmd := &cobra.Command{Use: "mcp-relay", Hidden: true, SilenceUsage: true, Args: cobra.MinimumNArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return workerruntime.RunRelay(relayToken, args, os.Stdin, os.Stdout, os.Stderr)
	}}
	relayCmd.Flags().StringVar(&relayToken, "token-file", "", "0600 file holding the session credential")
	cmd.AddCommand(relayCmd)
	var hub, code, label, mcpSource string
	var install bool
	var mailSocket string
	var mailUIDs []int
	var mailGID int
	enroll := &cobra.Command{Use: "enroll", RunE: func(c *cobra.Command, _ []string) error {
		if err := workerruntime.Enroll(c.Context(), root, hub, code, label); err != nil {
			return err
		}
		cfg, err := workerruntime.Load(root)
		if err != nil {
			return err
		}
		cfg.MailSocket = mailSocket
		cfg.MailUIDs = mailUIDs
		if mailGID >= 0 {
			cfg.MailGID = &mailGID
		}
		if err = workerruntime.SaveJSON(filepath.Join(root, "config.json"), cfg); err != nil {
			return err
		}
		if mcpSource != "" {
			if err := workerruntime.ImportHubMCP(root, mcpSource); err != nil {
				return err
			}
		}
		if install {
			binary, err := os.Executable()
			if err != nil {
				return err
			}
			if err := workerruntime.InstallService(c.Context(), root, binary); err != nil {
				return err
			}
		}
		fmt.Fprintln(c.OutOrStdout(), "enrolled", label)
		return nil
	}}
	enroll.Flags().StringVar(&hub, "hub", "", "hub HTTP(S) URL")
	enroll.Flags().StringVar(&code, "code", "", "one-time worker pairing code")
	enroll.Flags().StringVar(&label, "label", "", "worker label")
	enroll.Flags().StringVar(&mcpSource, "mcp-config", "", "existing JSON with configured hub torque/tesseract/tether clients")
	enroll.Flags().BoolVar(&install, "install", false, "install/start systemd user or launchd outpost service")
	enroll.Flags().StringVar(&mailSocket, "mail-socket", "", "local mail socket (cross-uid: parent must be group traversable)")
	enroll.Flags().IntSliceVar(&mailUIDs, "mail-allow-uid", nil, "uids allowed to register mail sessions (default own uid)")
	enroll.Flags().IntVar(&mailGID, "mail-gid", -1, "socket group id (must be one of outpost user's groups)")
	cmd.AddCommand(enroll)
	cmd.AddCommand(&cobra.Command{Use: "run", RunE: func(c *cobra.Command, _ []string) error {
		cfg, err := workerruntime.Load(root)
		if err != nil {
			return err
		}
		w := &workerruntime.Worker{Root: root, Config: cfg, Version: version}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		executor := workerruntime.NewExecutor(root, binary, w.Send)
		executor.Tokens = &workerruntime.TokenMinter{Hub: cfg.Hub, DeviceTokenFile: filepath.Join(root, "device.token")}
		if err := executor.Restore(); err != nil {
			return err
		}
		w.Inventory = executor.Inventory
		w.Hosts = executor.ScanHosts
		mail := &outpostmail.Server{Socket: cfg.MailSocket, State: filepath.Join(root, "mail.json"), AllowedUIDs: cfg.MailUIDs, GID: -1}
		if mail.Socket == "" {
			mail.Socket = filepath.Join(root, "run", "mail.sock")
		}
		if cfg.MailGID != nil {
			mail.GID = *cfg.MailGID
		}
		mail.Bind = func(ctx context.Context, b workerwire.Binding) (workerwire.Binding, error) {
			wait, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			r, err := w.Request(wait, workerwire.Frame{T: "bind", CmdID: uuid.NewString(), Binding: &b})
			if err != nil {
				if errors.Is(err, workerruntime.ErrHubDisconnected) || errors.Is(err, context.DeadlineExceeded) {
					return b, outpostmail.ErrHubOffline
				}
				return b, err
			}
			if r.Binding == nil {
				return b, fmt.Errorf("missing bind acknowledgement")
			}
			return *r.Binding, nil
		}
		mail.Unbind = func(ctx context.Context, b workerwire.Binding) error {
			wait, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, err := w.Request(wait, workerwire.Frame{T: "unbind", CmdID: uuid.NewString(), Binding: &b})
			return err
		}
		w.Bindings = mail.Inventory
		w.Command = func(ctx context.Context, f workerwire.Frame) error {
			if f.T == "welcome" {
				mail.Welcome(f.Bindings)
			}
			if f.T == "deliver" {
				wait, cancel := context.WithTimeout(ctx, 12*time.Second)
				defer cancel()
				err := mail.Deliver(wait, f)
				r := workerwire.Frame{T: "deliver_ack", CmdID: f.CmdID}
				if err != nil {
					r.Error = err.Error()
				}
				return w.Send(r)
			}
			return executor.Command(ctx, f)
		}
		ctx, cancel := context.WithCancel(c.Context())
		defer cancel()
		done := make(chan error, 2)
		go func() { done <- mail.Serve(ctx) }()
		go executor.ReapLoop(ctx, workerruntime.HostReapInterval, workerruntime.OrphanGrace)
		go func() { done <- w.Run(ctx) }()
		return <-done
	}})
	cmd.AddCommand(&cobra.Command{Use: "status", RunE: func(c *cobra.Command, _ []string) error {
		cfg, err := workerruntime.Load(root)
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(cfg)
	}})
	cmd.AddCommand(&cobra.Command{Use: "version", Run: func(c *cobra.Command, _ []string) { fmt.Fprintln(c.OutOrStdout(), version) }})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
