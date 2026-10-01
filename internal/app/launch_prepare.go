package app

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/cli/antigravity"
	"github.com/hollis-labs/tether/internal/store"
)

func muxCommandPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "mux"
}

// muxEnvMap is the environment the planted `mux mcp` is started with: its
// upstream allow-list. The plan's own list, or the default when it has none
// (launch.DefaultMCPServers), so a launched agent's proxy always carries one
// and, with --confine, never holds an upstream it was not granted. Only the
// daemon's session launches use it; `mux boot` plants for the operator's own
// terminal and keeps its own (cmd/mux muxEnvFromPlan).
func muxEnvMap(env map[string]string) map[string]string {
	return map[string]string{launch.MCPServersEnv: strings.Join(launch.EffectiveMCPServers(env), ",")}
}

// MuxMCPPlan is what a planting decided: the argv to write into the worker's
// .mcp.json, and what that argv means for the session's ref attribution.
//
// THE TWO TRAVEL TOGETHER ON PURPOSE. Returning only the argv would leave the
// caller to work out the attribution separately, and two independent
// statements of one decision are what drift — the recurring failure this
// sprint kept finding, where a component is correct in isolation and the
// composition root wires it differently. There is no way to hold an argv from
// one call and an attribution from another.
type MuxMCPPlan struct {
	// Args is the argv for the planted `mux mcp` server.
	Args []string
	// Attribution is one of store.RefAttribution{Proxy,None,Unlaunched}, to be
	// stamped on the session row once the planting has actually happened.
	Attribution string
}

// MuxMCPPlant builds the `mux mcp` planting for a launched worker's
// .mcp.json. Workers get a full-scope connection on purpose: an agent that
// cannot reply to a message or launch a session is not sandboxed, it is
// broken. The --token here is only the adapter's presence check — it is opaque
// and unvalidated (see docs/mcp.md), not a secret. Meaningful per-call
// authorization belongs in the broker, not in a launch-time scope flag.
//
// sessionID is threaded through as --session so the proxy knows WHICH session
// it is serving. Nothing else tells it: before CW-20260912-0074 the plumbing
// existed and was never connected — mcpadapter.WithSessionID was defined and
// called from nowhere, and every one of proxy_events' 2000 rows carried an
// empty session_id as a result. Without it the proxy can say a session called
// torque_task_get and not which task, and S3's extraction has no session to
// attach a ref to.
//
// Empty sessionID emits no flag, which is honest rather than defensive: a
// caller with no session genuinely has none to report, and an empty
// attribution is better than a fabricated one. It also reports
// RefAttributionUnlaunched, so a caller that DOES have a row to stamp records
// that this session's proxy can never attribute a call to it.
//
// A session the daemon launched also gets --protect-path for each directory
// Tether protects from its agent (the catalog root and the run directory), so
// the planted server refuses to write them whether or not a sandbox is around
// it: Codex spawns MCP servers itself, outside any Tether sandbox.
//
// A session the daemon launched also gets --daemon-only: its server runs
// inside the agent's sandbox, never opens the state database, and reaches
// Tether's state only through the daemon, so the sandbox can keep the state
// directory read-only (CW-20261001-0173). boot-exec (no session) keeps an
// ordinary server: it runs in the operator's own terminal, outside any
// Tether sandbox, and works without a daemon.
//
// extractRefs enables proxy-side identifier extraction (--extract-refs).
// Configured via catalog settings (CW-20260912-0112) and passed here from
// LaunchSession. The flag and the attribution stamp are decided together:
// the argv and the stamp cannot disagree.
func MuxMCPPlant(catalogRoot, sessionID string, extractRefs bool, protected ...string) MuxMCPPlan {
	args := []string{
		"--catalog", catalogRoot,
		"mcp", "--proxy",
		"--token", "tether-worker",
		"--scopes", "session.write,message.write,catalog.write",
	}
	if sessionID == "" {
		return MuxMCPPlan{Args: args, Attribution: store.RefAttributionUnlaunched}
	}
	// A session's proxy is confined to the upstreams it was granted
	// (MUX_MCP_SERVERS, see muxEnvMap): it loads, starts and exposes only
	// those, and mux_call cannot reach the rest (CW-20261001-0227). The
	// sessionless caller, `mux boot`, plants for an operator's own terminal
	// and is not confined.
	args = append(args, "--confine", "--daemon-only", "--session", sessionID)
	// The directories Tether protects from this agent are also refused by the
	// planted server itself (mux_agent_create / mux_agent_edit), as a policy,
	// because a runtime that spawns MCP servers outside Tether's sandbox (Codex)
	// would otherwise let the server write them on the agent's behalf
	// (CW-20261001-0142).
	for _, dir := range protected {
		args = append(args, "--protect-path", dir)
	}
	if !extractRefs {
		return MuxMCPPlan{Args: args, Attribution: store.RefAttributionNone}
	}
	args = append(args, "--extract-refs")
	return MuxMCPPlan{Args: args, Attribution: store.RefAttributionProxy}
}

func mergeEnv(base []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return base
	}
	out := append([]string(nil), base...)
	for k, v := range overlay {
		if k == "" {
			continue
		}
		prefix := k + "="
		replaced := false
		for i, kv := range out {
			if strings.HasPrefix(kv, prefix) {
				out[i] = prefix + v
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, prefix+v)
		}
	}
	return out
}

// withBrowserShim puts the antigravity browser shim first on an agy
// launch's PATH and leaves every other provider's env untouched. It is a
// soft guard: agy's browser sign-in finds `open` on PATH, so an expired
// login fails instead of opening a window. See antigravity.PlantBrowserShim
// for what it does not cover.
func withBrowserShim(providerBrand, workspaceRoot string, env []string) ([]string, error) {
	if providerBrand != "antigravity" {
		return env, nil
	}
	dir, err := antigravity.PlantBrowserShim(filepath.Join(workspaceRoot, antigravity.BrowserShimDirName))
	if err != nil {
		return nil, err
	}
	return antigravity.PrependPATH(env, dir), nil
}
