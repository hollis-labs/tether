package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hollis-labs/agentkit/agentlaunch"

	"github.com/hollis-labs/tether/internal/provider/cli/antigravity"
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
// extractRefs enables proxy-side identifier extraction (--extract-refs).
// Configured via catalog settings (CW-20260912-0112) and passed here from
// LaunchSession. The flag and the attribution stamp are decided together:
// the argv and the stamp cannot disagree.
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

// sharedExtraArgs is the part of the shared launch's argv the session runtime
// cannot compose for itself: the arguments that point into roots only the
// launch knows — the planted boot dir (claude's --mcp-config) and the project
// dir (claude's --add-dir, codex's --cd) — plus any injected args.
//
// INTERIM (CW-20261001-0015): CW-20260930-0135 (one argv owner) and
// CW-20260930-0106 (native launches on the wrapper) replace this. Until then
// two things compose argv. prepared.Argv is a whole provider argv — binary,
// the projection's launch convention (subcommand, output flags, the boot
// prompt as a positional), ProviderSpec.Flags, Injection.Args — while the
// runtime's PlanScopedAdapter prepends plan.Args to its own adapter's
// convention and agentkit appends ExtraArgs after that. Passing all of
// prepared.Argv[1:] through composed the convention twice: duplicated
// -p/--output-format for claude, `codex app-server app-server`, and a stray
// positional for `codex exec` (exit 2). The adapter keeps the convention, and
// only what it cannot know crosses over.
//
// Re-derived against go-providers v0.33.0 / agentkit v0.12.0
// (CW-20261001-0065): v0.33.0 makes go-providers the single owner of each
// argv convention, but Tether still composes twice. prepared.Argv is
// agentkit's projection (a print-mode convention with the boot prompt as a
// positional, whatever the runtime), and the session runtime's adapter
// builds its own argv per turn. So this narrowing still stands until
// CW-20260930-0106 puts launches on one prepared execution. With v0.33.0,
// claude's projected --add-dir <project> arrives in every mode and is kept
// here as a root-bound arg.
//
// claude, codex and opencode take the narrowed path; opencode's runtime
// adapter carries the planted --agent itself (opencode.New). antigravity
// keeps the old pass-through until CW-20261001-0095 retires this.
func sharedExtraArgs(providerBrand string, prepared *agentlaunch.PreparedLaunch, baseArgs []string) []string {
	if prepared == nil || len(prepared.Argv) < 2 {
		return nil
	}
	if providerBrand != "claude" && providerBrand != "codex" && providerBrand != "opencode" {
		return passThroughExtraArgs(prepared.Argv, baseArgs)
	}
	binding := prepared.Argv[1:]
	roots := []string{prepared.PlantedBootDir, prepared.WorkspaceDir, prepared.Workdir}
	var flags, injected []string
	if prepared.Compiled != nil {
		roots = append(roots, prepared.Compiled.ResolvedProjectRoot)
		if plan := prepared.Compiled.Plan; plan != nil {
			flags = plan.Provider.Flags
			injected = plan.Injection.Args
			roots = append(roots, plan.Workspace.Workdir, plan.Project.Root)
		}
	}
	// providerplant places ProviderSpec.Flags then Injection.Args right
	// before the projected argv's end-of-options "--" (agentkit v0.12.3), or
	// after the binding when there is none; peel them off wherever they are.
	tail := append(append([]string(nil), flags...), injected...)
	if n := peelIndex(binding, tail); n >= 0 {
		binding = slices.Delete(slices.Clone(binding), n, n+len(tail))
	} else {
		flags, injected = nil, nil
	}

	out := rootBoundArgs(binding, roots, flagTargets(baseArgs, prepared.Workdir))
	// The catalog engine's flags are plan.Args, which PlanScopedAdapter
	// already prepends; only flags it does not carry still need passing.
	if !slices.Equal(flags, baseArgs) {
		out = append(out, flags...)
	}
	return append(out, injected...)
}

// peelIndex is where seg sits in argv: immediately before the first "--",
// else at the end. -1 when it is in neither place.
func peelIndex(argv, seg []string) int {
	if i := slices.Index(argv, "--"); i >= 0 {
		if n := i - len(seg); n >= 0 && slices.Equal(argv[n:i], seg) {
			return n
		}
	}
	if n := len(argv) - len(seg); n >= 0 && slices.Equal(argv[n:], seg) {
		return n
	}
	return -1
}

// passThroughExtraArgs is the pre-CW-20261001-0015 behavior: all of argv
// after the binary, less a leading plan.Args the adapter already prepends.
func passThroughExtraArgs(argv, baseArgs []string) []string {
	args := argv[1:]
	if len(baseArgs) > 0 && len(args) >= len(baseArgs) && slices.Equal(args[:len(baseArgs)], baseArgs) {
		args = args[len(baseArgs):]
	}
	return append([]string(nil), args...)
}

// rootBoundArgs keeps the args that name a path under one of roots, each with
// the flag in front of it ("--mcp-config <bootdir>/.mcp.json"), except a pair
// already in have — the same flag at the same target.
func rootBoundArgs(args, roots []string, have map[string]bool) []string {
	var out []string
	prev := ""
	for _, arg := range args {
		switch {
		case !underAnyRoot(arg, roots):
		case strings.HasPrefix(prev, "-"):
			if !have[flagTarget(prev, arg, "")] {
				out = append(out, prev, arg)
			}
		default:
			out = append(out, arg)
		}
		prev = arg
	}
	return out
}

// flagTargets indexes each "--flag value" pair in args by flag and target,
// a relative value resolved against cwd. launch.Resolve splices
// `--mcp-config .mcp.json` into claude's plan.Args, relative to the boot dir
// the process starts in — the same file as the projection's absolute one.
func flagTargets(args []string, cwd string) map[string]bool {
	have := map[string]bool{}
	prev := ""
	for _, arg := range args {
		if strings.HasPrefix(prev, "-") && !strings.HasPrefix(arg, "-") {
			have[flagTarget(prev, arg, cwd)] = true
		}
		prev = arg
	}
	return have
}

func flagTarget(flag, value, cwd string) string {
	if !filepath.IsAbs(value) && cwd != "" {
		value = filepath.Join(cwd, value)
	}
	return flag + "\x00" + filepath.Clean(value)
}

func underAnyRoot(path string, roots []string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
