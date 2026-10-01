package config

import (
	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	permission "github.com/hollis-labs/go-permission"
)

// Permission postures for launched agents, in Tether's own vocabulary.
//
//   - PermissionModeDefault — the agent runs under its provider's
//     conservative posture and is refused what needs approval.
//   - PermissionModeBypass  — the agent runs without tool-permission
//     prompts; for claude, `--permission-mode bypassPermissions`.
//
// ProviderPosture maps each onto the go-permission posture a launch carries.
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

// EffectivePermissionMode resolves the permission posture for a launched
// agent. Precedence: the agent's permissions.permission_mode overrides the
// global defaults.permission_mode; an empty result falls back to
// PermissionModeDefault.
func EffectivePermissionMode(global Global, agent LaunchProfile) string {
	if m := agent.Permissions.PermissionMode; m != "" {
		return m
	}
	if m := global.Catalog.Defaults.PermissionMode; m != "" {
		return m
	}
	return PermissionModeDefault
}

// ProviderPosture maps a Tether permission mode onto the go-permission
// posture a launch of providerBrand in mode carries (agentlaunch's
// ProviderSpec.Permission and RuntimeBinding.Permission). agentkit turns the
// posture into the provider's own flags and environment through the
// go-providers registry, and refuses a provider's own spelling. An empty
// posture means none: the provider runs under its own defaults. An empty
// permissionMode is PermissionModeDefault.
//
// Every row keeps the posture each provider ran under before postures
// existed:
//
//	provider     bypass   default
//	claude       yolo     default
//	codex        accept-edits (both)
//	antigravity  yolo     accept-edits
//	opencode     none (both)
//	other / ACP  none
func ProviderPosture(permissionMode, providerBrand string, mode runtimes.Mode) permission.Mode {
	if mode.ACP() {
		// An ACP agent's permission requests are answered by the ACP
		// client; go-providers has no launch posture for an ACP mode.
		return ""
	}
	bypass := permissionMode == PermissionModeBypass
	switch providerBrand {
	case "claude":
		if bypass {
			return permission.ModeYolo
		}
		return permission.ModeDefault
	case "codex":
		// codex bypass→yolo deliberately not used: danger-full-access +
		// approval never would cut off MCP (see internal/app/codex_approval.go);
		// revisit with a posture that's full-access + on-request.
		return permission.ModeAcceptEdits
	case "antigravity":
		if bypass {
			return permission.ModeYolo
		}
		return permission.ModeAcceptEdits
	case "opencode":
		// No posture: opencode keeps its own defaults, which allow edit,
		// bash and webfetch and, under `run`, auto-reject what they ask for
		// (external_directory, doom_loop, .env reads). yolo would allow
		// external_directory and doom_loop too, and every other posture
		// makes bash ask, which `run` auto-rejects.
		return ""
	default:
		return ""
	}
}
