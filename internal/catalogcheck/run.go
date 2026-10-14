package catalogcheck

import (
	"fmt"

	"github.com/hollis-labs/tether/internal/config"
)

// LoadAndValidate loads the catalog at root and runs the validation `tether
// doctor` opens with: the layered load, MCP grants against the declared
// servers (the cause of the 2026-10 outage) and structural validation. It
// returns the loaded catalog (nil on failure) and the result.
func LoadAndValidate(root string) (*config.Catalog, Result) {
	cat, err := config.LoadLayered(root)
	if err != nil {
		return nil, Fail("catalog-present", fmt.Sprintf("load failed: %v", err), "run: tether init")
	}
	if err := cat.ValidateMCPGrants(); err != nil {
		return nil, Fail("catalog-mcp-grants", err.Error(), "fix the named grant or enable its upstream under mcp-servers/")
	}
	if err := cat.Validate(); err != nil {
		return nil, Fail("catalog-valid", fmt.Sprintf("validation failed: %v", err), "check your catalog YAML files or run: tether init --force")
	}
	return cat, OK("catalog-present", root)
}

// Run checks the catalog tree at root and returns structured findings:
// failures first, each with a severity, a check name, an entity when the
// message names one, and a remedy. It reads files only; it never contacts a
// daemon, a database or an upstream, and it does not modify anything.
func Run(root string) Report {
	root = config.Expand(root)
	cat, first := LoadAndValidate(root)
	results := []Result{first}
	if cat != nil {
		results = append(results, SandboxProfiles(cat), MCPDiscoveryMode(cat))
		results = append(results, MCPProfiles(cat, root)...)
		results = append(results, MCPNamingConfig(root)...)
		results = append(results, MCPCredentialFiles(root)...)
		results = append(results, TeamsPolicy(cat, root)...)
		results = append(results, ManagerBypass(cat)...)
	}
	return newReport(results)
}
