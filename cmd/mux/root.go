package main

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var catalogPath string

var rootCmd = &cobra.Command{
	Use:   "mux",
	Short: "Agent Mux — local agent session control plane",
}

func init() {
	defaultCatalog := filepath.Join(os.Getenv("HOME"), ".agent-mux", "catalog")
	rootCmd.PersistentFlags().StringVar(&catalogPath, "catalog", defaultCatalog, "catalog root directory")
	rootCmd.AddCommand(projectsCmd, agentsCmd, resolveCmd, launchCmd, sessionsCmd, daemonCmd, tuiCmd)
}
