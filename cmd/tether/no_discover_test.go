package main

import (
	"context"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/spf13/cobra"
	"testing"
)

func TestNoDiscoverSelectsFlatAndRejectsConflict(t *testing.T) {
	old := mcpDiscoveryMode
	t.Cleanup(func() { mcpDiscoveryMode = old })
	t.Setenv("TETHER_MCP_DISCOVERY_MODE", "search")
	for _, explicit := range []string{"", "flat", "search"} {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.Flags().Bool("no-discover", false, "")
		cmd.Flags().StringVar(&mcpDiscoveryMode, "discovery-mode", "", "")
		if err := cmd.Flags().Set("no-discover", "true"); err != nil {
			t.Fatal(err)
		}
		if explicit != "" {
			if err := cmd.Flags().Set("discovery-mode", explicit); err != nil {
				t.Fatal(err)
			}
		}
		in, err := resolveMCPModeInputs(cmd, &app.Service{Catalog: &config.Catalog{}}, nil)
		if explicit == "search" {
			if err == nil {
				t.Fatal("conflicting explicit mode accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		mode, err := mcpgateway.ResolveMode(in)
		if err != nil || mode.Mode != mcpgateway.Flat {
			t.Fatalf("mode=%+v %v", mode, err)
		}
	}
}
