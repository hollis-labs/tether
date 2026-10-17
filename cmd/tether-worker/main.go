package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/libs/harness/core/shim"
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
	cmd.AddCommand(mailCommand(&root))
	var providerConfig string
	providerCmd := &cobra.Command{Use: "provider-process", Hidden: true, RunE: func(c *cobra.Command, _ []string) error {
		return workerruntime.RunProviderProcess(c.Context(), providerConfig, os.Stdin, os.Stdout, os.Stderr)
	}}
	providerCmd.Flags().StringVar(&providerConfig, "config", "", "private prepared provider configuration")
	cmd.AddCommand(providerCmd)
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
	var httpURL, httpToken string
	httpRelayCmd := &cobra.Command{Use: "mcp-http-relay", Hidden: true, SilenceUsage: true, Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		r := &workerruntime.HTTPRelay{URL: httpURL, TokenFile: httpToken, Stderr: os.Stderr}
		return r.Run(c.Context(), os.Stdin, os.Stdout)
	}}
	httpRelayCmd.Flags().StringVar(&httpURL, "url", "", "hub streamable-HTTP MCP endpoint (https)")
	httpRelayCmd.Flags().StringVar(&httpToken, "token-file", "", "0600 file holding the session's current issuer token (re-read for every request)")
	cmd.AddCommand(httpRelayCmd)
	var hub, code, label, mcpSource string
	var workerPath, workspaceRoot string
	var providerCommands map[string]string
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
		if c.Flags().Changed("path") {
			cfg.Path = workerPath
		}
		if c.Flags().Changed("workspace-root") {
			cfg.WorkspaceRoot = workspaceRoot
		}
		if c.Flags().Changed("provider-command") {
			cfg.ProviderCommands = providerCommands
		}
		cfg.MailUIDs = mailUIDs
		if mailGID >= 0 {
			cfg.MailGID = &mailGID
		}
		if err = cfg.Validate(); err != nil {
			return err
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
	enroll.Flags().StringVar(&workerPath, "path", "", "worker executable search PATH, saved in config.json")
	enroll.Flags().StringVar(&workspaceRoot, "workspace-root", "", "absolute worker workspace root")
	enroll.Flags().StringToStringVar(&providerCommands, "provider-command", nil, "worker provider command overrides (brand=command)")
	enroll.Flags().StringVar(&code, "code", "", "one-time worker pairing code")
	enroll.Flags().StringVar(&label, "label", "", "worker label")
	enroll.Flags().StringVar(&mcpSource, "mcp-config", "", "existing JSON with configured hub torque/tesseract/tether clients")
	enroll.Flags().BoolVar(&install, "install", false, "install/start systemd user or launchd outpost service")
	enroll.Flags().StringVar(&mailSocket, "mail-socket", "", "local mail socket (cross-uid: parent must be group traversable)")
	enroll.Flags().IntSliceVar(&mailUIDs, "mail-allow-uid", nil, "uids allowed to register mail sessions (default own uid)")
	enroll.Flags().IntVar(&mailGID, "mail-gid", -1, "socket group id (must be one of outpost user's groups)")
	cmd.AddCommand(enroll)
	cmd.AddCommand(&cobra.Command{Use: "run", RunE: func(c *cobra.Command, _ []string) error {
		runtime, err := workerruntime.NewRuntime(root, version)
		if err != nil {
			return err
		}
		return runtime.Run(c.Context(), nil)
	}})
	cmd.AddCommand(pluginCommand())
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
