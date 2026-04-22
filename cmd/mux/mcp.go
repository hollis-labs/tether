package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/app"
	"github.com/chrispian/agent-mux/internal/mcpadapter"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the MCP stdio adapter",
	Long: `Start an MCP stdio adapter that exposes the agent-mux runtime as tools.

The adapter communicates over stdin/stdout using the MCP protocol. Configure
your MCP client (Claude Desktop, Cursor, etc.) to run this command.

Mutating tools (session.create/launch/stop, message.send, etc.) require a
token and the corresponding scope. Pass them via flags or environment
variables:

  AGENT_MUX_MCP_TOKEN=<token> AGENT_MUX_MCP_SCOPES=session.write,message.write \
    mux mcp

Available scopes:
  session.write  — create, launch, stop, wait, input, resize, resume sessions
  message.write  — send, consume, cancel messages

Example MCP client config (mcp.json):
  {
    "mcpServers": {
      "agent-mux": {
        "command": "mux",
        "args": ["mcp"],
        "env": {
          "AGENT_MUX_MCP_TOKEN": "your-token",
          "AGENT_MUX_MCP_SCOPES": "session.write,message.write"
        }
      }
    }
  }`,
	RunE: runMCP,
}

var (
	mcpToken  string
	mcpScopes string
	mcpProxy  bool
)

func init() {
	mcpCmd.Flags().StringVar(&mcpToken, "token", "", "auth token for mutating tools (env: AGENT_MUX_MCP_TOKEN)")
	mcpCmd.Flags().StringVar(&mcpScopes, "scopes", "", "comma-separated scopes: session.write,message.write (env: AGENT_MUX_MCP_SCOPES)")
	mcpCmd.Flags().BoolVar(&mcpProxy, "proxy", false, "enable MCP proxy mode: load upstream servers from catalog/mcp-servers/ and merge their tools")
}

func runMCP(cmd *cobra.Command, _ []string) error {
	// Flags take precedence; fall back to env vars.
	token := mcpToken
	if token == "" {
		token = os.Getenv("AGENT_MUX_MCP_TOKEN")
	}
	scopeStr := mcpScopes
	if scopeStr == "" {
		scopeStr = os.Getenv("AGENT_MUX_MCP_SCOPES")
	}
	scopes := splitScopes(scopeStr)

	svc, err := app.New(expandCatalogPath())
	if err != nil {
		return fmt.Errorf("init service: %w", err)
	}
	defer func() { _ = svc.Close() }()

	adapter := mcpadapter.New(svc, token, scopes)
	if mcpProxy {
		// Phase 2: wire observability — LoggingMiddleware + ToolCallEventStore.
		store := mcpadapter.NewToolCallEventStore(1000)
		opts := mcpadapter.ProxyOptions{
			Bus:        svc.Bus,
			EventStore: store,
		}
		return adapter.RunWithProxyOpts(cmd.Context(), expandCatalogPath(), opts)
	}
	return adapter.Run(cmd.Context())
}

func splitScopes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
