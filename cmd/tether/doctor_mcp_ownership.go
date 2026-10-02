package main

import (
	"context"
	"fmt"
	"github.com/hollis-labs/tether/internal/config"
	"time"
)

func checkMCPUpstreamOwnership(cat *config.Catalog) checkResult {
	if cat == nil {
		return warn("mcp-upstream-ownership", "catalog unavailable", "fix catalog first")
	}
	mode, err := cat.Global.Daemon.MCPUpstreamOwnership()
	if err != nil {
		return fail("mcp-upstream-ownership", err.Error(), "choose legacy_proxy or daemon in global.yaml; existing sessions retain their planted ownership")
	}
	message := fmt.Sprintf("%s; identity=%s; applies to new launches only", mode, cat.Global.Identity.EffectiveMode())
	if cfg, err := daemonConfigFromCatalog(cat); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		health, err := daemonClient(cfg.ListenAddr).Health(ctx)
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
