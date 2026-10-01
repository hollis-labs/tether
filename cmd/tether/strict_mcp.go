package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
)

// logClaudeStrictMCP records at daemon startup whether the Claude agents it
// launches load only the MCP servers Tether plants (CW-20261001-0227). Strict
// mode that is off, which only the operator's TETHER_CLAUDE_STRICT_MCP=0 can
// do, is logged as a warning, so it is never silent.
func logClaudeStrictMCP(logf func(string, ...any), st app.StrictMCPStatus) {
	if st.Enabled {
		logf("claude strict MCP %s", st.Reason)
		return
	}
	logf("WARN: claude strict MCP %s", st.Reason)
}

// checkClaudeStrictMCP reports whether Tether-launched Claude agents load only
// the MCP servers Tether plants. The daemon decides from its own environment,
// so the check asks the running daemon (/health); the doctor's own
// environment only answers when no daemon is running. Strict mode that is off
// warns: agents then also load user-level servers from ~/.claude.json,
// project .mcp.json files and claude.ai connectors.
func checkClaudeStrictMCP(cat *config.Catalog, local app.StrictMCPStatus) checkResult {
	const name = "claude-strict-mcp"
	if cat == nil {
		return warn(name, "skipped — catalog unavailable", "fix catalog first")
	}
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil {
		return warn(name, fmt.Sprintf("cannot resolve the daemon's listen addr: %v", err), "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := client.New(cfg.ListenAddr).Health(ctx)
	return strictMCPResult(h, err, local)
}

// strictMCPResult decides the check from the daemon's /health answer (err is
// non-nil when there was none) and the doctor's own environment.
func strictMCPResult(h daemon.Health, healthErr error, local app.StrictMCPStatus) checkResult {
	const name = "claude-strict-mcp"
	off := func(reason string) checkResult {
		return warn(name, reason, fmt.Sprintf("unset %s in tetherd's environment and restart the daemon", app.ClaudeStrictMCPEnv))
	}
	switch {
	case healthErr != nil:
		// No daemon to ask: say what one started from this environment would do.
		if !local.Enabled {
			return off("daemon not running; one started from this environment would have it " + local.Reason)
		}
		return ok(name, "daemon not running; one started from this environment would run Claude agents strict")
	case h.Hardening == nil:
		return warn(name, "the running daemon predates strict MCP, so its Claude agents also load user-level and claude.ai MCP servers",
			"restart the daemon on a build that includes CW-20261001-0227")
	case !h.Hardening.ClaudeStrictMCP:
		return off(h.Hardening.ClaudeStrictMCPReason)
	}
	return ok(name, h.Hardening.ClaudeStrictMCPReason)
}

// localStrictMCPStatus is the decision the doctor's own environment gives.
func localStrictMCPStatus() app.StrictMCPStatus { return app.ClaudeStrictMCP(os.Getenv) }
