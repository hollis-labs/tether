package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var catalogPath string
var tokenFilePath string

var rootCmd = &cobra.Command{
	Use:     "tether",
	Short:   "Tether — local agent session control plane",
	Version: version,
}

func init() {
	// os.UserHomeDir is cross-platform (HOME on unix, USERPROFILE on Windows)
	// and the right primitive here. If it fails (no home env), the default
	// degrades to a relative ".tether/catalog" — callers must then pass
	// --catalog explicitly rather than silently hitting the wrong path.
	home, _ := os.UserHomeDir()
	defaultCatalog := filepath.Join(home, ".tether", "catalog")
	rootCmd.PersistentFlags().StringVar(&tokenFilePath, "token-file", "", "bearer credential file (overrides TETHER_TOKEN and operator.token)")
	rootCmd.PersistentFlags().StringVar(&catalogPath, "catalog", defaultCatalog, "catalog root directory")
	rootCmd.SetVersionTemplate(fmt.Sprintf("tether %s (commit %s, built %s)\n", version, commit, buildDate))
	rootCmd.AddCommand(projectsCmd, agentsCmd, resolveCmd, launchCmd, sessionsCmd, daemonCmd, serveCmd, workspacesCmd, bootPromptsCmd, mcpCmd, acpCmd, messagesCmd, aiCmd, eventsCmd)
	// Top-level aliases for discoverability.
	rootCmd.AddCommand(generateBootCmd, listBootProfilesCmd, bootLaunchCmd, bootExecCmd)
	rootCmd.AddCommand(pathCmd())
	rootCmd.AddCommand(settingsCmd())
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(detectCmd, doctorCmd)
	rootCmd.AddCommand(newWorkerServiceCommand())
	rootCmd.AddCommand(workstreamsCmd)
}
