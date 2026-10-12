package main

import (
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tether/internal/outpostmail"
	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
)

func mailCommand(root *string) *cobra.Command {
	cmd := &cobra.Command{Use: "mail", Short: "Credential-free local Outpost mail registration"}
	var socket string
	cmd.PersistentFlags().StringVar(&socket, "socket", "", "Outpost mail socket (or TETHER_MAIL_SOCKET)")
	var q outpostmail.Request
	attach := &cobra.Command{Use: "attach", Short: "Register an unmanaged agent and execute its local delivery adapter", RunE: func(c *cobra.Command, _ []string) error {
		path := socket
		if path == "" {
			path = os.Getenv("TETHER_MAIL_SOCKET")
		}
		if path == "" {
			cfg, err := workerruntime.Load(*root)
			if err == nil {
				path = cfg.MailSocket
			}
		}
		if path == "" {
			path = filepath.Join(*root, "run", "mail.sock")
		}
		return outpostmail.Attach(c.Context(), path, q, c.OutOrStdout())
	}}
	attach.Flags().StringVar(&q.Name, "name", "", "worker-local agent name for a minted URN")
	attach.Flags().StringVar(&q.URN, "urn", "", "existing URN (requires hub grant)")
	attach.Flags().StringVar(&q.Adapter, "adapter", "", "tmux-paste or codex-queue")
	attach.Flags().StringVar(&q.Handle, "handle", "", "tmux pane id or Codex thread id")
	attach.Flags().IntVar(&q.PID, "pid", os.Getppid(), "agent process pid (checked with OS start-time and uid)")
	attach.Flags().StringSliceVar(&q.AllowedSenders, "allow-sender", nil, "exact sender URNs allowed to wake (operator always allowed)")
	attach.Flags().BoolVar(&q.EscapeFirst, "escape-first", false, "tmux: send Escape before paste (Codex TUI)")
	cmd.AddCommand(attach)
	ack := &cobra.Command{Use: "ack", Short: "Acknowledge native mod acceptance (private JSON on stdin)", RunE: func(c *cobra.Command, _ []string) error {
		path := socket
		if path == "" {
			path = os.Getenv("TETHER_MAIL_SOCKET")
		}
		if path == "" {
			return fmt.Errorf("mail socket required")
		}
		var q outpostmail.Request
		if err := json.NewDecoder(io.LimitReader(c.InOrStdin(), 1<<20)).Decode(&q); err != nil {
			return err
		}
		return outpostmail.Acknowledge(c.Context(), path, q)
	}}
	cmd.AddCommand(ack)
	var gids int
	var uids []int
	configure := &cobra.Command{Use: "configure", Short: "Configure the socket/uid boundary on an enrolled Outpost (restart to apply)", RunE: func(c *cobra.Command, _ []string) error {
		cfg, err := workerruntime.Load(*root)
		if err != nil {
			return err
		}
		cfg.MailSocket = socket
		cfg.MailUIDs = uids
		cfg.MailGID = &gids
		return workerruntime.SaveJSON(filepath.Join(*root, "config.json"), cfg)
	}}
	configure.Flags().IntSliceVar(&uids, "allow-uid", nil, "allowed peer uids (default own uid)")
	configure.Flags().IntVar(&gids, "gid", os.Getgid(), "socket group id")
	cmd.AddCommand(configure)
	return cmd
}
