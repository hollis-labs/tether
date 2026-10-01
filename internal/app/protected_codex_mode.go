package app

// CodexProtection is how Tether protects the codex agents it launches.
type CodexProtection string

const (
	// CodexGuarded: codex runs under its own workspace-write sandbox, and
	// Tether's allowlist (codexOwnsSandbox) is judged at every launch and
	// re-checked before every turn. A launch that fails it is wrapped in
	// Tether's own sandbox instead, which fails loudly where that cannot start.
	// DORMANT: it is not the shipped state, see codexProtectionMode.
	CodexGuarded CodexProtection = "guarded"

	// CodexNotProtected is the shipped state: codex runs exactly as it did
	// before control-plane protection existed, under its own workspace-write
	// sandbox, with none of Tether's guard on its launch or turns, and /health
	// and `tether doctor` say so. The catalog-writing tether tools are still refused
	// for it, because that policy is the planted server's and does not depend
	// on this switch.
	CodexNotProtected CodexProtection = "not protected"
)

// codexProtectionMode is THE SWITCH for how codex is protected, and it ships as
// CodexNotProtected: codex is NOT protected by Tether's write-protection of the
// catalog, run/ and state/. In this mode protectionPlan leaves a codex launch alone (no
// allowlist, no wrapping, no refusal inside a protected directory), no
// per-turn check is registered, the daemon reports "codex: not protected
// (CW-20261001-0230)", and claude, opencode and the rest stay protected. The
// planted `tether mcp` is still started with --protect-path, so codex still cannot
// write the catalog through tether_agent_create or tether_agent_edit. Its planted local
// proxy and stdio descendants are wrapped separately (CW-20261001-0466).
//
// The guard (codexOwnsSandbox, the per-turn re-check, Plan.CallerEnv, the
// config.toml line validator, the MCP-grant rule) stays in the tree, DORMANT
// until CW-20261001-0230 makes it sound. Three adversarial reviews found it
// could not be made sound while codex spawns MCP servers outside its sandbox.
// What it would need before it is switched on:
//
//   - B1: the default {torque, tesseract} list is not safe for an exempted
//     codex. `torque mcp` boots agents in-process, and torque_session_launch
//     takes a caller-chosen workdir, so workdir=<catalog> boots a codex whose
//     own workspace-write root is the catalog (CW-20261001-0464). Likewise
//     loom_export_bundle(dir=...) writes into an arbitrary directory
//     (CW-20261001-0465). An allowlist of upstreams cannot be sound until
//     upstreams run daemon-side, or confine what they write.
//   - SF4: the per-turn check accepts any content inside [mcp_servers.*], so a
//     co-located wrapped agent can add [mcp_servers.evil] command="sh", drop
//     --protect-path from the planted tether args, or add LD_PRELOAD, and codex
//     spawns it unsandboxed. The planted shape is known at launch and would
//     have to be pinned.
//
// It is a variable only so a test can drive both states.
var codexProtectionMode = CodexNotProtected

// CodexProtectionState is what /health and `tether doctor` report about codex.
type CodexProtectionState struct {
	State  string
	Reason string
}

// codexProtectionState describes how codex is protected, for protection that
// is enabled, and says so plainly when it is not.
func codexProtectionState(st ProtectionStatus) CodexProtectionState {
	if !st.Enabled {
		return CodexProtectionState{State: "not applicable", Reason: "control-plane protection is off, so no agent, codex included, is write-protected"}
	}
	switch codexProtectionMode {
	case CodexNotProtected:
		return CodexProtectionState{
			State:  string(CodexNotProtected),
			Reason: "not protected (CW-20261001-0230): codex runs under its own workspace-write sandbox and spawns MCP servers outside that sandbox. Tether wraps the planted local proxy and its stdio descendants to protect catalog, run/ and state/ (CW-20261001-0466); remote upstreams are excluded unless the operator opts in. The MCP config is not pinned and caller identity is pending (CW-20260930-0253), so substituted servers or host services can still reach the catalog (including torque_session_launch and loom_export_bundle); the tether tools that write the catalog are still refused for it. Claude and OpenCode agents are protected",
		}
	default:
		return CodexProtectionState{
			State:  string(CodexGuarded),
			Reason: "guarded (dormant guard switched on): codex runs under its own workspace-write sandbox, and Tether judges its flags, environment, injection, project config and MCP list at every launch and re-checks them before every turn, wrapping any launch it cannot vouch for; codex still spawns MCP servers outside that sandbox, so an MCP tool can reach the catalog (CW-20261001-0230)",
		}
	}
}
