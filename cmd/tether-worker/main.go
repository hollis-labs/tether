package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/spf13/cobra"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

var version = "dev"

func main() {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".tether-worker")
	cmd := &cobra.Command{Use: "tether-worker", Short: "Thin execution outpost for the Tether hub"}
	cmd.PersistentFlags().StringVar(&root, "root", root, "worker state root")
	var hub, code, label string
	enroll := &cobra.Command{Use: "enroll", RunE: func(c *cobra.Command, _ []string) error {
		if err := workerruntime.Enroll(c.Context(), root, hub, code, label); err != nil {
			return err
		}
		fmt.Fprintln(c.OutOrStdout(), "enrolled", label)
		return nil
	}}
	enroll.Flags().StringVar(&hub, "hub", "", "hub HTTP(S) URL")
	enroll.Flags().StringVar(&code, "code", "", "one-time worker pairing code")
	enroll.Flags().StringVar(&label, "label", "", "worker label")
	cmd.AddCommand(enroll)
	cmd.AddCommand(&cobra.Command{Use: "run", RunE: func(c *cobra.Command, _ []string) error {
		cfg, err := workerruntime.Load(root)
		if err != nil {
			return err
		}
		w := &workerruntime.Worker{Root: root, Config: cfg, Version: version}
		return w.Run(c.Context())
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
