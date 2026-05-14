package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/acpadapter"
	"github.com/hollis-labs/tether/internal/acpsvc"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
)

var acpCmd = &cobra.Command{
	Use:   "acp",
	Short: "Start the Agent Client Protocol (ACP) stdio adapter",
	Long: `Start an ACP stdio adapter that exposes a mux session to ACP-aware editors
(Zed, JetBrains via acp.json, Avante.nvim, CodeCompanion.nvim).

The adapter speaks JSON-RPC 2.0 over newline-delimited stdio. Each editor
spawns ` + "`mux acp --agent <launch_id>`" + ` as a subprocess; mux launches a session
under the named launch profile and streams agent output back as ACP
session/update notifications during each prompt turn.

Authentication mirrors ` + "`mux mcp`" + `: an optional bearer token + scopes. With
auth enabled the editor must call the ACP authenticate method with
{"token":"<bearer>"} before any session/* calls. With no token (development
only) all scope-gated handlers run unauthenticated.

ACP MVP method coverage (v005-09):
  initialize, authenticate, session/new, session/prompt, session/cancel,
  session/close, session/resume + outbound session/update notifications.

Example editor configurations:

  Zed (settings.json):
    "agent_servers": {
      "Mux": {
        "command": "mux",
        "args": ["acp", "--agent", "claude-stream"],
        "env": {"AGENT_MUX_ACP_TOKEN": "your-token"}
      }
    }

  JetBrains (acp.json):
    {"command": "mux", "args": ["acp", "--agent", "claude-stream"]}
`,
	RunE: runACP,
}

var (
	acpToken    string
	acpScopes   string
	acpAgentID  string
	acpAgentSrv string
)

func init() {
	acpCmd.Flags().StringVar(&acpToken, "token", "",
		"auth token for scope-gated handlers (env: AGENT_MUX_ACP_TOKEN)")
	acpCmd.Flags().StringVar(&acpScopes, "scopes", "session.write",
		"comma-separated scopes (env: AGENT_MUX_ACP_SCOPES). Default: session.write")
	acpCmd.Flags().StringVar(&acpAgentID, "agent", "",
		"mux launch profile ID (REQUIRED). Picks which agent the editor drives via this ACP connection.")
	acpCmd.Flags().StringVar(&acpAgentSrv, "agent-name", "mux",
		"agentInfo.name returned in ACP initialize response. Default: mux")
}

func runACP(cmd *cobra.Command, _ []string) error {
	token := acpToken
	if token == "" {
		token = os.Getenv("AGENT_MUX_ACP_TOKEN")
	}
	scopeStr := acpScopes
	if scopeStr == "" {
		scopeStr = os.Getenv("AGENT_MUX_ACP_SCOPES")
	}
	scopes := splitACPScopes(scopeStr)

	if acpAgentID == "" {
		return fmt.Errorf("acp: --agent <launch_id> is required (no implicit default — see `mux launch list`)")
	}

	// Initialize the in-process service so we can resolve catalog state
	// even though this subcommand routes everything mutating through
	// the daemon. acpsvc itself only consumes internal/client.Client;
	// app.Service exists here so future read-only paths (e.g., catalog
	// inspection during initialize) have a handle if needed.
	svc, err := app.New(expandCatalogPath())
	if err != nil {
		return fmt.Errorf("init service: %w", err)
	}
	defer func() { _ = svc.Close() }()

	// Resolve the daemon's address. ACP MVP requires the daemon — there
	// is no in-process fallback for session lifecycle here (unlike the
	// mcpadapter rescue, which retains an in-process path for tests).
	listenAddr, _ := resolveDaemonAddr()
	if listenAddr == "" {
		return fmt.Errorf("acp: cannot resolve daemon address (start with `mux daemon up` and check catalog config)")
	}
	dc := client.New(listenAddr)

	// Route the adapter's warn-level logs to stderr so they don't
	// pollute the stdout protocol stream.
	stderrLogger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	bridge := acpsvc.New(dc, acpAgentID, stderrLogger)
	adapter := acpadapter.New(bridge, token, scopes,
		acpadapter.WithAgentName(acpAgentSrv),
		acpadapter.WithLogger(stderrLogger),
	)

	return adapter.Run(cmd.Context(), os.Stdin, os.Stdout)
}

// splitACPScopes parses a comma-separated scope list, trimming whitespace
// and dropping empties. Mirrors splitScopes in mcp.go but kept local so
// the two subcommands evolve independently.
func splitACPScopes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
