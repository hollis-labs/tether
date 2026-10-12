package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/substrate/harness/shim"
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
	var hub, code, label, mcpSource string
	var install bool
	enroll := &cobra.Command{Use: "enroll", RunE: func(c *cobra.Command, _ []string) error {
		if err := workerruntime.Enroll(c.Context(), root, hub, code, label); err != nil {
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
		if err := executor.Restore(); err != nil {
			return err
		}
		w.Inventory = executor.Inventory
		w.Command = executor.Command
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
