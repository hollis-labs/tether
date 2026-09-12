package app

import (
	"os"
	"strings"
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

// MuxMCPArgs builds the argv for the `mux mcp` server planted into a launched
// worker's .mcp.json. Workers get a full-scope connection on purpose: an agent
// that cannot reply to a message or launch a session is not sandboxed, it is
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
// attribution is better than a fabricated one.
func MuxMCPArgs(catalogRoot, sessionID string) []string {
	args := []string{
		"--catalog", catalogRoot,
		"mcp", "--proxy",
		"--token", "tether-worker",
		"--scopes", "session.write,message.write,catalog.write",
	}
	if sessionID != "" {
		args = append(args, "--session", sessionID)
	}
	return args
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
