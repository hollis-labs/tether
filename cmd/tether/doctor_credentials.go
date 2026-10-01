package main

import "github.com/hollis-labs/tether/internal/config"

func checkMCPCredentialFiles(catalogRoot string) []checkResult {
	checks, err := config.CheckMCPServerCredentialFiles(config.Expand(catalogRoot))
	if err != nil {
		return []checkResult{fail("mcp-credential-files", err.Error(), "fix the MCP server catalog YAML")}
	}
	if len(checks) == 0 {
		return []checkResult{ok("mcp-credential-files", "no enabled MCP credential file references")}
	}
	results := make([]checkResult, 0, len(checks))
	for _, check := range checks {
		name := "mcp-credential-file:" + check.ServerID + ":" + check.Field
		if check.Err != nil {
			results = append(results, fail(name, check.Err.Error(), "use a private regular credential file owned by you; symlinks must resolve inside the catalog or home"))
		} else {
			results = append(results, ok(name, "credential file is usable"))
		}
	}
	return results
}
