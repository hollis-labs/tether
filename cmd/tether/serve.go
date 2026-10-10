package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
)

// serve invokes the existing foreground daemon; it owns no second process or
// service graph. Its listener overrides exist only for this invocation.
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the daemon in the foreground with loopback remote access",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemonRunCmd.RunE(cmd, args)
	},
}

func addRemoteServeFlags(cmd *cobra.Command) {
	cmd.Flags().String("remote-listen", "", "Enable remote listener at tcp:loopback-IP:port")
	cmd.Flags().StringSlice("allowed-host", nil, "Exact remote HTTP Host authorities (including forwarded ports)")
	cmd.Flags().StringSlice("allowed-origin", nil, "Exact remote HTTP(S) browser origins; default is same HTTP origin")
}

func daemonConfigFromCommand(cat *config.Catalog, cmd *cobra.Command) (daemon.Config, error) {
	// Copy the catalog value so CLI flags never write back to configuration.
	copyCatalog := *cat
	remote := cat.Global.Daemon.RemoteListener
	if cmd.Name() == "serve" || cmd.Flags().Changed("remote-listen") {
		remote.Enabled = true
		addr, err := cmd.Flags().GetString("remote-listen")
		if err != nil {
			return daemon.Config{}, err
		}
		if cmd.Flags().Changed("remote-listen") && addr == "" {
			return daemon.Config{}, fmt.Errorf("--remote-listen requires a loopback TCP address")
		}
		if addr != "" {
			remote.ListenAddr = addr
		}
	}
	if cmd.Flags().Changed("allowed-host") {
		remote.AllowedHosts, _ = cmd.Flags().GetStringSlice("allowed-host")
	}
	if cmd.Flags().Changed("allowed-origin") {
		remote.AllowedOrigins, _ = cmd.Flags().GetStringSlice("allowed-origin")
	}
	copyCatalog.Global.Daemon.RemoteListener = remote
	return daemonConfigFromCatalog(&copyCatalog)
}

func init() {
	addRemoteServeFlags(serveCmd)
}
