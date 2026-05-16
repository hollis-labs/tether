package config

// Claude Code permission postures for launched agents.
//
//   - PermissionModeDefault — the agent starts in claude's `default`
//     permission mode and prompts before tool use.
//   - PermissionModeBypass  — the agent is launched with
//     `--dangerously-skip-permissions`; no tool-permission prompts.
//
// The conservative value is PermissionModeDefault: it is the code-level
// fallback when neither the agent nor the global default sets a mode, so
// a missing or empty config never silently bypasses permissions.
const (
	PermissionModeDefault = "default"
	PermissionModeBypass  = "bypass"
)

// ValidPermissionMode reports whether m is a recognized permission mode.
// The empty string is valid — it means "inherit" at the agent level and
// "fall back to default" at the global level.
func ValidPermissionMode(m string) bool {
	switch m {
	case "", PermissionModeDefault, PermissionModeBypass:
		return true
	default:
		return false
	}
}

// EffectivePermissionMode resolves the Claude Code permission posture for
// a launched agent. Precedence: the agent's permissions.permission_mode
// overrides the global defaults.permission_mode; an empty result falls
// back to PermissionModeDefault.
func EffectivePermissionMode(global Global, agent Agent) string {
	if m := agent.Permissions.PermissionMode; m != "" {
		return m
	}
	if m := global.Catalog.Defaults.PermissionMode; m != "" {
		return m
	}
	return PermissionModeDefault
}
