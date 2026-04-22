package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/tui"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Open the interactive TUI",
	Long: `Open the keyboard-driven interactive TUI for Agent Mux.

Daily-usable surface for browsing the catalog, launching sessions,
observing running sessions, and editing catalog objects. Requires a
running muxd daemon; start one with 'mux daemon start' if you see a
connection error on open.

Press q or Ctrl-C to quit.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadDaemonConfig(catalogPath)
		if err != nil {
			return fmt.Errorf("load daemon config: %w", err)
		}
		return tui.Run(tui.Options{
			ListenAddr:  cfg.ListenAddr,
			CatalogRoot: expandCatalogPath(),
		})
	},
}
