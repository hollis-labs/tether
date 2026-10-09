package main

import (
	"fmt"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/environment"
)

func checkRoleModules(cat *config.Catalog) []checkResult {
	profile, err := cat.Global.Profile()
	if err != nil {
		return []checkResult{fail("role", err.Error(), "set role: worker or hub and use known module names")}
	}
	role := profile.Role
	if profile.Legacy {
		role += " (legacy combined defaults)"
	}
	checks := []checkResult{ok("role", role)}
	for _, name := range environment.ModuleNames() {
		status := "off"
		if profile.Enabled(name) {
			status = "on"
		}
		check := ok("module:"+name, status)
		if profile.Enabled(name) {
			switch name {
			case environment.EnvironmentDirectory, environment.ConnectionManager:
				check = warn("module:"+name, "configured on; implementation not installed", "available in a later worker-environments slice")
			case environment.RemoteListener:
				check.Message = "on; legacy single listener (remote per-listener policy not installed)"
			case environment.LocalMCP:
				if !cat.Global.Daemon.MCPEndpoint.Enabled {
					check.Message = "on; daemon HTTP endpoint remains opted out"
				}
			case environment.LLMGateway:
				check.Message = fmt.Sprintf("on; %d configured providers (runtime helper availability checked at startup)", len(cat.Global.AI.Providers))
			}
		}
		checks = append(checks, check)
	}
	return checks
}
