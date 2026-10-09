package main

import (
	"strings"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/teamcli"
	"github.com/spf13/cobra"
)

// buildRootCommand reads the same default-off catalog key as the host.
// The CLI gate affects discovery only; the daemon remains the authority.
func buildRootCommand(root *cobra.Command, args []string, fallback string) *cobra.Command {
	catalogRoot := commandCatalogRoot(args, fallback)
	enabled := false
	if cat, err := config.Load(catalogRoot); err == nil {
		if profile, err := cat.Global.Profile(); err == nil {
			enabled = profile.Enabled(environment.Teams)
		}
	}
	for _, cmd := range root.Commands() {
		if cmd.Name() == "team" {
			root.RemoveCommand(cmd)
		}
	}
	if cmd := teamcli.NewCommand(defaultRegistryClientFactory, enabled); cmd != nil {
		root.AddCommand(cmd)
	}
	return root
}

// commandCatalogRoot reads the persistent catalog selector before Cobra builds
// its enabled command tree. It does not alter any flag or identity state.
func commandCatalogRoot(args []string, fallback string) string {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--catalog=") {
			fallback = strings.TrimPrefix(arg, "--catalog=")
		}
		if arg == "--catalog" && i+1 < len(args) {
			fallback = args[i+1]
		}
	}
	return fallback
}
