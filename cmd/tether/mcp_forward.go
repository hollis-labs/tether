package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpforward"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func runMCPForwardDaemon(cmd *cobra.Command) error {
	cmd.SilenceUsage = true
	if mcpProxy || mcpDaemonOnly || mcpConfine || mcpExtractRefs || mcpScopes != "" || mcpServers != "" || mcpOnly != "" || len(mcpProtect) != 0 || mcpToken != "" {
		return fmt.Errorf("--forward-daemon cannot be combined with local adapter/proxy flags")
	}
	if !strings.HasPrefix(mcpDaemonAddress, "unix:/") {
		return fmt.Errorf("--forward-daemon requires explicit --daemon-address unix:/absolute/socket")
	}
	// This route never consults the catalog or operator.token, even when a
	// caller omits --session. An explicit token file or session env are allowed.
	token, err := forwardDaemonCredential()
	if err != nil {
		return err
	}
	opts := client.MCPOptions{}
	if len(mcpProfiles) > 1 {
		return fmt.Errorf("--forward-daemon accepts one --profile")
	}
	profile, present := os.LookupEnv("TETHER_MCP_PROFILE")
	if len(mcpProfiles) == 1 {
		profile, present = mcpProfiles[0], true
	}
	if present {
		opts.Profile = &profile
	}
	mode, present := os.LookupEnv("TETHER_MCP_DISCOVERY_MODE")
	selectors := explicitMCPModes(cmd)
	var inherited *string
	if present {
		inherited = &mode
	}
	selection, err := mcpgateway.ResolveMode(mcpgateway.ModeInputs{Explicit: selectors, Environment: inherited})
	if err != nil {
		return err
	}
	if len(selectors) > 0 {
		mode, present = string(selection.Mode), true
	}
	if present {
		opts.DiscoveryMode = &mode
	}
	return mcpforward.Run(cmd.Context(), client.New(mcpDaemonAddress, client.WithToken(token)), opts, &mcp.StdioTransport{})
}

func forwardDaemonCredential() (string, error) {
	if tokenFilePath != "" {
		token, err := identity.ReadTokenFile(tokenFilePath)
		if err != nil {
			return "", fmt.Errorf("read forwarding credential: %w", err)
		}
		return token, nil
	}
	return os.Getenv("TETHER_TOKEN"), nil
}
