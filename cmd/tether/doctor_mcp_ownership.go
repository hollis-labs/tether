package main

import (
	"context"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/config"
)

func checkMCPUpstreamOwnership(cat *config.Catalog) checkResult {
	if cat == nil {
		return warn("mcp-upstream-ownership", "catalog unavailable", "fix catalog first")
	}
	mode, err := cat.Global.Daemon.MCPUpstreamOwnership()
	if err != nil {
		return fail("mcp-upstream-ownership", err.Error(), "choose legacy_proxy or daemon in global.yaml; existing sessions retain their planted ownership")
	}
	if mode == config.MCPUpstreamsDaemon && !cat.Global.Daemon.MCPEndpoint.Enabled {
		return fail("mcp-upstream-ownership", "daemon ownership requires the MCP endpoint, but it is disabled", "enable daemon.mcp_endpoint.enabled and restart the daemon, or select legacy_proxy")
	}
	message := fmt.Sprintf("%s; identity=%s; applies to new launches only", mode, cat.Global.Identity.EffectiveMode())
	if cfg, err := daemonConfigFromCatalog(cat); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		dc := daemonClient(cfg.ListenAddr)
		if cat.Global.Daemon.MCPEndpoint.Enabled {
			if err := dc.ProbeMCP(ctx); err != nil {
				return warn("mcp-upstream-ownership", message+"; endpoint configured enabled but health/admission unavailable", "check daemon restart, identity mode and caller credential; select legacy_proxy while unavailable")
			}
			message += "; endpoint healthy (verified admission, no upstream initialization)"
		} else {
			message += "; endpoint disabled"
		}
		health, err := dc.Health(ctx)
		if err == nil && health.Hardening != nil && health.Hardening.MCPUpstreamSessions != nil {
			counts := health.Hardening.MCPUpstreamSessions
			message += fmt.Sprintf("; active sessions legacy_proxy=%d daemon=%d unknown=%d", counts["legacy_proxy"], counts["daemon"], counts["unknown"])
			recorder := health.Hardening.MCPRecorder
			if recorder["dropped"] > 0 || recorder["failures"] > 0 {
				return warn("mcp-upstream-ownership", message+fmt.Sprintf("; recorder dropped=%d failures=%d", recorder["dropped"], recorder["failures"]), "check daemon load and state database availability")
			}
		}
	}
	return ok("mcp-upstream-ownership", message)
}
