package catalogcheck

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/mcptransport"
)

// checkSandboxProfiles fails when an agent names a sandbox profile the
// catalog does not define. The daemon still starts, but refuses every
// launch of that agent (CW-20261001-0130), so it is a failure here.
func SandboxProfiles(cat *config.Catalog) Result {
	issues := cat.SandboxIssues()
	if len(issues) == 0 {
		return OK("catalog-sandbox-profiles", fmt.Sprintf("every agent's sandbox profile is defined (%d profiles)", len(cat.SandboxProfiles)))
	}
	msgs := make([]string, len(issues))
	for i, issue := range issues {
		msgs[i] = issue.Error()
	}
	return Fail("catalog-sandbox-profiles", strings.Join(msgs, "; "),
		"add the missing profile under sandbox-profiles/ or fix the agent's permissions.default_sandbox, then restart the daemon")
}

func MCPDiscoveryMode(cat *config.Catalog) Result {
	if _, err := mcpgateway.ResolveMode(mcpgateway.ModeInputs{Gateway: cat.Global.MCP.DiscoveryMode}); err != nil {
		return Fail("mcp-discovery-mode", err.Error(), "fix mcp.discovery_mode and mcp.profiles fields in global.yaml")
	}
	return OK("mcp-discovery-mode", "configured discovery modes are valid")
}

// Doctor validates all authored profiles without connecting upstreams or reading
// credentials. Exact order/load names can only be verified against live tools.
func MCPProfiles(cat *config.Catalog, catalogRoot string) []Result {
	entries, err := config.LoadMCPServerCatalog(catalogRoot)
	if err != nil {
		return []Result{Fail("mcp-profiles", err.Error(), "fix the MCP server catalog")}
	}
	known := map[string]bool{"tether": true}
	for _, entry := range entries {
		if entry.ID != "tether" {
			known[entry.ID] = entry.IsEnabled()
		}
	}
	var results []Result
	for id, profile := range cat.Global.MCP.Profiles {
		name := "mcp-profile-" + id
		if id == "" {
			results = append(results, Fail(name, "empty profile ID", "name the profile"))
			continue
		}
		if err := profile.Validate(); err != nil {
			results = append(results, Fail(name, err.Error(), "fix profile fields in global.yaml"))
			continue
		}
		if _, err := mcpgateway.SelectOrigins(known, nil, &profile); err != nil {
			results = append(results, Fail(name, err.Error(), "use enabled catalog origins or reserved native tether"))
			continue
		}

		collision := false
		for _, id := range profile.Servers {
			if id == "tether" {
				for _, entry := range entries {
					if entry.ID == "tether" {
						collision = true
					}
				}
			}
		}
		if collision {
			results = append(results, Fail(name, "reserved native tether origin conflicts with catalog upstream tether", "rename the upstream before selecting the reserved native origin"))
			continue
		}
		if len(profile.Order)+len(profile.AlwaysLoad) > 0 {
			results = append(results, Warn(name, "order/always_load tool names require live upstream discovery", "start the selected gateway profile to validate exact names"))
		} else {
			results = append(results, OK(name, "profile syntax and origins are valid"))
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results
}

func MCPNamingConfig(root string) []Result {
	entries, err := config.LoadMCPServerCatalog(config.Expand(root))
	if err != nil {
		return []Result{Fail("mcp-naming", "cannot read MCP server catalog", "fix catalog YAML")}
	}
	var out []Result
	ids := []string{}
	for _, entry := range entries {
		if entry.IsEnabled() {
			if entry.ID == "tether" {
				out = append(out, Warn("mcp-origins", mcpgateway.ValidateOriginIDs([]string{"tether"}).Error(), "rename the upstream ID; startup fails only when this upstream is selected"))
				continue
			}
			ids = append(ids, entry.ID)
		}
	}
	if err := mcpgateway.ValidateOriginIDs(ids); err != nil {
		out = append(out, Fail("mcp-origins", err.Error(), "fix enabled upstream IDs; tether is reserved"))
	}
	for _, entry := range entries {
		if entry.ToolPrefixInvalid {
			out = append(out, Warn("mcp-prefix:"+entry.ID, entry.CatalogFile+": tool_prefix must be a string; invalid value ignored", "declare a string tool_prefix"))
		}
		if !entry.IsEnabled() {
			continue
		}
		if entry.ToolPrefix != "" {
			for _, finding := range mcpgateway.LintName(entry.ID, entry.ToolPrefix) {
				out = append(out, Warn("mcp-prefix:"+entry.ID, finding.Message, "fix declared tool_prefix; live names require --mcp-live"))
			}
		}
	}
	if len(out) == 0 {
		out = append(out, OK("mcp-naming", "catalog origin/prefix declarations checked; live collision/name/metadata checks require --mcp-live"))
	}
	return out
}

func MCPEndpoint(cat *config.Catalog) Result {
	if profile, err := cat.Global.Profile(); err == nil && !profile.Enabled(environment.LocalMCP) {
		return OK("mcp-endpoint", "disabled by modules.local_mcp")
	}
	if !cat.Global.Daemon.MCPEndpoint.Enabled {
		return OK("mcp-endpoint", "disabled (daemon.mcp_endpoint.enabled defaults false)")
	}
	addr := cat.Global.Daemon.ListenAddr
	if addr == "" {
		addr = "unix:~/.tether/run/tetherd.sock"
	}
	if strings.HasPrefix(addr, "unix:") {
		addr = "unix:" + config.Expand(strings.TrimPrefix(addr, "unix:"))
	}
	if err := mcptransport.ValidateEndpoint(addr, identity.Mode(cat.Global.Identity.ModeFor("mcp_gateway"))); err != nil {
		return Fail("mcp-endpoint", err.Error(), "fix daemon.listen_addr or disable daemon.mcp_endpoint.enabled")
	}
	return OK("mcp-endpoint", "enabled; strict verified admission on /mcp and /p/<profile>")
}

func MCPCredentialFiles(catalogRoot string) []Result {
	checks, err := config.CheckMCPServerCredentialFiles(config.Expand(catalogRoot))
	if err != nil {
		return []Result{Fail("mcp-credential-files", err.Error(), "fix the MCP server catalog YAML")}
	}
	if len(checks) == 0 {
		return []Result{OK("mcp-credential-files", "no enabled MCP credential file references")}
	}
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		name := "mcp-credential-file:" + check.ServerID + ":" + check.Field
		if check.Err != nil {
			results = append(results, Fail(name, check.Err.Error(), "use a private regular credential file owned by you; symlinks must resolve inside the catalog or home"))
		} else {
			results = append(results, OK(name, "credential file is usable"))
		}
	}
	return results
}

func RoleModules(cat *config.Catalog) []Result {
	profile, err := cat.Global.Profile()
	if err != nil {
		return []Result{Fail("role", err.Error(), "set role: worker or hub and use known module names")}
	}
	role := profile.Role
	if profile.Legacy {
		role += " (legacy combined defaults)"
	}
	checks := []Result{OK("role", role)}
	for _, name := range environment.ModuleNames() {
		status := "off"
		if profile.Enabled(name) {
			status = "on"
		}
		check := OK("module:"+name, status)
		if profile.Enabled(name) {
			switch name {
			case environment.EnvironmentDirectory, environment.ConnectionManager:
				check = Warn("module:"+name, "configured on; implementation not installed", "available in a later worker-environments slice")
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
