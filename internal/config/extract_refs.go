package config

// EffectiveExtractRefs resolves the proxy-side ref extraction posture for a
// launched session.
//
// Precedence:
//  1. launch.MCP.ExtractRefs (if explicitly set)
//  2. proj.MCP.ExtractRefs (if explicitly set)
//  3. global.Catalog.Defaults.ExtractRefs (fleet-wide default)
//  4. false (off by default)
func EffectiveExtractRefs(global Global, proj LaunchContext, launch Launch) bool {
	if launch.MCP.ExtractRefs != nil {
		return *launch.MCP.ExtractRefs
	}
	if proj.MCP.ExtractRefs != nil {
		return *proj.MCP.ExtractRefs
	}
	return global.Catalog.Defaults.ExtractRefs
}
