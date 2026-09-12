package app

import (
	"os"
	"strings"

	"github.com/hollis-labs/tether/internal/store"
)

func muxCommandPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "mux"
}

func muxEnvMap(env map[string]string) map[string]string {
	if env == nil || env["MUX_MCP_SERVERS"] == "" {
		return nil
	}
	return map[string]string{"MUX_MCP_SERVERS": env["MUX_MCP_SERVERS"]}
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
// extractRefs is currently false at every call site and there is no config
// seam that would make it true (CW-20260912-0112). The parameter exists anyway
// rather than being hardcoded, because the point of this function is that the
// flag and the stamp are decided in one place: when the seam lands it changes
// one argument, not two files that have to agree.
func MuxMCPPlant(catalogRoot, sessionID string, extractRefs bool) MuxMCPPlan {
	args := []string{
		"--catalog", catalogRoot,
		"mcp", "--proxy",
		"--token", "tether-worker",
		"--scopes", "session.write,message.write,catalog.write",
	}
	if sessionID == "" {
		return MuxMCPPlan{Args: args, Attribution: store.RefAttributionUnlaunched}
	}
	args = append(args, "--session", sessionID)
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

func sharedExtraArgs(argv, baseArgs []string) []string {
	if len(argv) == 0 {
		return nil
	}
	args := argv[1:]
	if len(baseArgs) > 0 && len(args) >= len(baseArgs) {
		matches := true
		for i := range baseArgs {
			if args[i] != baseArgs[i] {
				matches = false
				break
			}
		}
		if matches {
			args = args[len(baseArgs):]
		}
	}
	return append([]string(nil), args...)
}
