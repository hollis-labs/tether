package app

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/hollis-labs/agentkit/agentlaunch"
)

// Claude strict MCP (CW-20261001-0227).
//
// Claude Code loads MCP servers from several places beside the one config a
// launch hands it with --mcp-config: the operator's user-level
// ~/.claude.json, project-scoped .mcp.json files, and the claude.ai
// connectors on the logged-in account. Tether plants exactly one MCP config
// per agent (its own `tether` proxy), so anything else Claude loads is a server
// Tether did not put there. Today the planted entry is named `tether` and
// shadows the one user-level `tether` entry, which is why a second proxy does
// not appear; any user-level server with another name would load into every
// Tether-launched agent.
//
// --strict-mcp-config tells Claude to use only --mcp-config's servers. Tether
// adds it to every Claude launch it makes through the shared launch template
// (the streaming, subprocess and pty runtimes, and the resume path, since all
// of them compile through agentLaunchPlanFor), unless the operator turns it
// off. `tether boot` is not covered: it runs the operator's own interactive
// Claude in their terminal, with their own servers.
//
// This is an interim measure. A go-agent-wrapper / agentkit option will
// replace it, and the flag goes when that lands.

// ClaudeStrictMCPEnv names the daemon environment variable that turns strict
// MCP off: "0" or "false" in tetherd's environment. It is on by default;
// turning it off is an operator decision the daemon logs at startup and
// `tether doctor` reports, never a silent one.
const ClaudeStrictMCPEnv = "TETHER_CLAUDE_STRICT_MCP"

// claudeStrictMCPFlag is Claude Code's flag; see the file comment.
const claudeStrictMCPFlag = "--strict-mcp-config"

// StrictMCPStatus says whether the Claude agents Tether launches load only
// the MCP servers Tether plants, and why.
type StrictMCPStatus struct {
	Enabled bool
	// DisabledByOperator is set when ClaudeStrictMCPEnv turned strict mode off.
	DisabledByOperator bool
	// Reason says what the status means for an agent, for logs, doctor and
	// the daemon's /health.
	Reason string
}

// ClaudeStrictMCP decides strict MCP for a daemon environment.
func ClaudeStrictMCP(getenv func(string) string) StrictMCPStatus {
	raw := getenv(ClaudeStrictMCPEnv)
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false":
		return StrictMCPStatus{DisabledByOperator: true, Reason: fmt.Sprintf("DISABLED by %s=%s, so Claude agents also load MCP servers from the operator's ~/.claude.json, project .mcp.json files and claude.ai connectors", ClaudeStrictMCPEnv, raw)}
	}
	return StrictMCPStatus{Enabled: true, Reason: "on: Claude agents load only the MCP servers Tether plants"}
}

// defaultStrictMCPStatus is the daemon's decision, taken from its environment.
var defaultStrictMCPStatus = func() StrictMCPStatus { return ClaudeStrictMCP(os.Getenv) }

// ClaudeStrictMCPStatus is the strict-MCP decision launches use. The daemon
// reports it at startup and through /health.
func (s *Service) ClaudeStrictMCPStatus() StrictMCPStatus {
	if s.strictMCPStatus != nil {
		return s.strictMCPStatus()
	}
	return defaultStrictMCPStatus()
}

// applyClaudeStrictMCP adds --strict-mcp-config to a Claude launch plan's
// flags, once, while strict MCP is on. Flags land at the launch template's
// extra-argument slot, so the flag is in every turn's argv, not only the
// first.
func (s *Service) applyClaudeStrictMCP(lp *agentlaunch.LaunchPlan) {
	if lp.Provider.ID != "claude" || !s.ClaudeStrictMCPStatus().Enabled {
		return
	}
	if slices.Contains(lp.Provider.Flags, claudeStrictMCPFlag) {
		return
	}
	lp.Provider.Flags = append(append([]string(nil), lp.Provider.Flags...), claudeStrictMCPFlag)
}
