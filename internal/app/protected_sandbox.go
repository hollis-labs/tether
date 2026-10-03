package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	permission "github.com/hollis-labs/go-permission"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// ProbeBwrap checks that bubblewrap can build the sandbox control-plane
// protection uses, around dir: unprivileged user and PID namespaces, the host
// filesystem, and dir read-only. A bwrap on PATH is not enough: where
// unprivileged user namespaces are restricted (AppArmor's
// kernel.apparmor_restrict_unprivileged_userns=1) it exists and still fails
// with "setting up uid map: Permission denied". Linux only; elsewhere it
// returns nil.
func ProbeBwrap(dir string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return fmt.Errorf("bwrap not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--unshare-user", "--unshare-pid", "--dev-bind", "/", "/", "--ro-bind", dir, dir, "true").CombinedOutput() //nolint:gosec // G204: fixed argv; dir is Tether's own catalog root
	if err != nil {
		return fmt.Errorf("bwrap cannot build a protecting sandbox (%w): %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// bwrapProbeTTL bounds how stale a cached probe may be. Launches and /health
// both ask; the answer changes only when the host does.
const bwrapProbeTTL = 30 * time.Second

var bwrapProbeCache struct {
	mu  sync.Mutex
	dir string
	err error
	at  time.Time
}

func cachedBwrapProbe(dir string) error {
	c := &bwrapProbeCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir == dir && !c.at.IsZero() && time.Since(c.at) < bwrapProbeTTL {
		return c.err
	}
	c.dir, c.err, c.at = dir, ProbeBwrap(dir), time.Now()
	return c.err
}

// ProtectionHealth is what the daemon reports about control-plane
// protection: the decision, and where Tether must sandbox agents itself,
// whether this host can.
type ProtectionHealth struct {
	ProtectionStatus
	// Codex says how codex is protected: guarded by the allowlist, or not
	// protected (the fallback). See codexProtectionMode.
	Codex CodexProtectionState
	// BwrapChecked is set when the host was probed: protection is on, on
	// Linux.
	BwrapChecked bool
	BwrapUsable  bool
	BwrapError   string
	// SkippedProjectLayers are registered projects left out of protection
	// because their repo_root cannot be used on this host AND no agent could
	// create it either: nothing from the nearest existing directory above it up
	// to / is owned by or writable by the daemon's user, who is the agent. There
	// is nothing to plant into. The launches of every other project still work;
	// each entry is a catalog problem to fix.
	SkippedProjectLayers []config.SkippedProjectLayer
	// CreatedProjectRoots are registered projects whose repo_root is a placeholder:
	// it was missing, an agent could have created it, so protection created it
	// holding only an anchored .tether and a marker, so a protected agent cannot
	// plant a layer. Listed while the placeholder is all there is, including after
	// a restart. Each is a stale catalog entry to fix.
	CreatedProjectRoots []config.CreatedProjectRoot
	// AnchoredProjectAncestors are registered projects whose repo_root is missing
	// under a directory that is not writable but that an agent can get past (it
	// runs as the user who owns it, or can write the directory above it), so
	// protection anchored that directory read-only. Each is a stale catalog entry to
	// fix; launches of every other project work.
	AnchoredProjectAncestors []config.AnchoredAncestor
	// PlanError is why Tether cannot work out what to protect, when it cannot:
	// every launch it must protect is refused until it is fixed. It is NOT a
	// bubblewrap problem, and BwrapChecked/BwrapUsable still say what the probe
	// found, so the two are never mistaken for each other.
	PlanError string
}

// ComputeProtectionHealth combines a protection decision with a probe of the
// host. probe is called with the catalog root dir (the probe binds it
// read-only) only when protection is on, on Linux.
func ComputeProtectionHealth(st ProtectionStatus, goos, probeDir string, probe func(string) error) ProtectionHealth {
	h := ProtectionHealth{ProtectionStatus: st, Codex: codexProtectionState(st)}
	if !st.Enabled || goos != "linux" {
		return h
	}
	h.BwrapChecked = true
	if probeDir == "" {
		h.BwrapError = "the catalog root is not set"
		return h
	}
	if err := probe(probeDir); err != nil {
		h.BwrapError = err.Error()
		return h
	}
	h.BwrapUsable = true
	return h
}

// ProtectionHealth reports this Service's protection and whether the host can
// provide it.
func (s *Service) ProtectionHealth() ProtectionHealth {
	st := defaultProtectionStatus()
	if s.protectionStatus != nil {
		st = s.protectionStatus()
	}
	// The probe needs only the catalog root, so it runs whether or not the full
	// plan can be worked out: a plan failure used to leave this empty and report
	// "the catalog root is not set", blaming bubblewrap for a catalog problem.
	probeDir := ""
	var prepared config.CatalogProtection
	planErr := ""
	if st.Enabled {
		if root := config.Expand(s.CatalogRoot); root != "" {
			if resolved, err := realDir(root); err == nil {
				probeDir = resolved
			}
		}
		var err error
		if _, prepared, err = s.controlPlane(config.ProtectionOptions{}); err != nil {
			planErr = err.Error()
		}
	}
	h := ComputeProtectionHealth(st, runtime.GOOS, probeDir, bwrapAvailable)
	h.SkippedProjectLayers = prepared.Skipped
	h.CreatedProjectRoots = prepared.CreatedRoots
	h.AnchoredProjectAncestors = prepared.AnchoredAncestors
	h.PlanError = planErr
	sort.Slice(h.CreatedProjectRoots, func(i, j int) bool { return h.CreatedProjectRoots[i].Project < h.CreatedProjectRoots[j].Project })
	return h
}

// codexExtraWritableRoots lists the directories codex's workspace-write
// sandbox makes writable beyond a launch's own work directories: /tmp, and
// $TMPDIR from the agent's environment (or the daemon's, when the launch has
// none yet). A variable so tests can run without their temp dirs in the way.
var codexExtraWritableRoots = func(env []string) []string {
	roots := []string{"/tmp"}
	tmp := os.Getenv("TMPDIR")
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
			tmp = v
		}
	}
	if tmp != "" {
		roots = append(roots, tmp)
	}
	return roots
}

// codexOwnsSandbox reports whether a codex launch is confined by codex's own
// workspace-write sandbox in a way that excludes every protected directory,
// so Tether need not wrap it (CW-20261001-0142). It is an ALLOWLIST: the
// exemption holds only while everything that shapes codex's sandbox is
// known-safe, and any element it does not recognize, or that a caller could
// have supplied, takes it away. The reason is returned for the log.
//
// What shapes codex's sandbox, every one of which widens it in real codex
// 0.159.x (CW-20261001-0142 review and live probes):
//
//   - its flags: -c sandbox_workspace_write.writable_roots=[...], a
//     sandbox_mode of danger-full-access (attached forms included),
//     --add-dir (a relative one resolves against codex's cwd), -s, -p;
//   - the environment: any CODEX_* variable but CODEX_HOME, and a CODEX_HOME
//     outside the session's own boot dir;
//   - config files: CODEX_HOME/config.toml, which an injected boot_dir_overlay
//     or raw native file can write, and a project .codex/config.toml in the
//     work directory or any directory above it;
//   - the working directory: a directory that contains a protected one
//     becomes a writable root (real codex with cwd above the catalog wrote
//     it), and so do /tmp and $TMPDIR.
//
// A caller with session.write can reach all of these through tether_session_create
// (agent_inline provider_overrides, injection, override.env), so none of them
// may be trusted to be absent: each is checked, and the launch is wrapped in
// Tether's own sandbox when any is present. Where that sandbox cannot start,
// the launch fails loudly, which is the fail-closed outcome.
//
// opts may be nil, as at session create, where only what the plan itself
// shapes can be judged.
func codexOwnsSandbox(plan *launch.Plan, opts *agentsessions.StartOptions, protected []string) (bool, string) {
	if plan == nil {
		return false, "there is no launch plan"
	}
	if why := codexPlanUnsafe(plan); why != "" {
		return false, why
	}
	if opts == nil {
		return true, ""
	}
	if why := codexEnvUnsafe(opts.Env, opts.WorkspaceDir); why != "" {
		return false, why
	}
	dirs := launchDirs(plan, opts)
	for _, d := range dirs {
		if d.what == "workspace" {
			continue
		}
		if why := codexProjectConfigPresent(d.path); why != "" {
			return false, why
		}
	}
	home := codexHomeFromEnv(opts.Env)
	if why := codexConfigUnsafe(filepath.Join(home, "config.toml")); why != "" {
		return false, why
	}
	// CODEX_HOME inside a writable root: the agent could write its own
	// config.toml there in one turn and have the next turn read it. Codex's
	// writable roots are its working directory (--cd, the project's work or
	// repo root) and the temp dirs; its process directory and the workspace
	// are not.
	for _, root := range codexWritableRoots(plan, opts.Env) {
		if pathWithin(realPathOrClean(root), realPathOrClean(home)) {
			return false, fmt.Sprintf("CODEX_HOME %s is inside %s, a directory codex's sandbox lets the agent write, so it could plant its own config.toml", home, root)
		}
	}
	roots := append([]launchDir(nil), dirs...)
	for _, r := range codexExtraWritableRoots(opts.Env) {
		roots = append(roots, launchDir{"temp directory", r})
	}
	for _, root := range roots {
		resolved := realPathOrClean(root.path)
		for _, p := range protected {
			if pathWithin(resolved, p) || pathWithin(p, resolved) {
				return false, fmt.Sprintf("the %s %s overlaps the protected directory %s, so codex's sandbox would let it write there", root.what, root.path, p)
			}
		}
	}
	return true, ""
}

// launchDir is a directory a launch's agent works in, and what it is.
type launchDir struct{ what, path string }

// launchDirs lists every directory a launch's agent works in. The prepared
// process directory (opts.Workdir) is not the only one: Codex's process cwd is
// its boot dir, while --cd, and so its working directory, is the project's
// work root (or its repo root), which is where its sandbox's writable root and
// its project config come from. Empty paths are dropped.
func launchDirs(plan *launch.Plan, opts *agentsessions.StartOptions) []launchDir {
	var out []launchDir
	add := func(what, path string) {
		if path == "" {
			return
		}
		for _, d := range out {
			if d.path == path {
				return
			}
		}
		out = append(out, launchDir{what, path})
	}
	add("work directory", opts.Workdir)
	if plan != nil {
		add("project work root", plan.EffectiveWorkRoot())
		add("project repo root", plan.RepoRoot)
	}
	add("workspace", opts.WorkspaceDir)
	return out
}

// codexPlanUnsafe returns why the launch plan alone rules out the exemption,
// or "" when nothing in it does.
func codexPlanUnsafe(plan *launch.Plan) string {
	// The exemption rests on the accept-edits posture, which plants
	// sandbox_mode="workspace-write" (see config.ProviderPosture). If that
	// mapping ever changes, the exemption must not follow it silently.
	mode, _, ok := config.RuntimeMode(plan.RuntimeKind)
	if !ok || config.ProviderPosture(plan.PermissionMode, plan.ProviderBrand, mode) != permission.ModeAcceptEdits {
		return "codex does not run under the workspace-write posture the exemption relies on"
	}
	if codexSandboxWeakened(plan.Args) {
		return "the launch's flags switch codex's sandbox off or widen it"
	}
	if flag, bad := codexUnrecognisedArg(plan.Args); bad {
		return fmt.Sprintf("the launch passes %s, which is not known to leave codex's sandbox intact", flag)
	}
	if len(plan.CallerEnv) > 0 {
		return "the launch carries environment variables from a caller or an agent definition (" + strings.Join(plan.CallerEnv, ", ") + "), and variables such as PATH, TMPDIR and LD_PRELOAD defeat codex's sandbox"
	}
	if id := unsafeMCPUpstream(plan); id != "" {
		return "the planted MCP allow-list names " + id + ", which is not known to be free of host-exec and arbitrary file-write tools, and codex spawns MCP servers outside its sandbox (only " + strings.Join(codexSafeMCPUpstreams, " and ") + " are)"
	}
	if len(plan.BootDirOverlay) > 0 {
		return "the launch injects boot_dir_overlay files, which can write codex's config.toml"
	}
	for _, f := range plan.NativeFiles {
		if f.Kind == "skill" {
			continue
		}
		if rel := path.Clean(filepath.ToSlash(f.RelPath)); !strings.HasPrefix(rel, "skills/") {
			return "the launch injects a native file outside skills/, which can write codex's config.toml"
		}
	}
	return ""
}

// codexUnrecognisedArg returns the first catalog or caller flag that is not
// on the allowlist. Only flags that cannot touch codex's sandbox pass: the
// model, and -c/--config overrides of a short list of keys whose values keep
// the sandbox as it is. Anything else, attached short forms (-cK=V, -mV)
// included, is unrecognized. The flag's name is returned, never its value.
func codexUnrecognisedArg(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--model" || a == "-m":
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return a, true
			}
		case strings.HasPrefix(a, "--model="):
			if strings.HasPrefix(strings.TrimPrefix(a, "--model="), "-") {
				return "--model", true
			}
		case a == "-c" || a == "--config":
			i++
			if i >= len(args) || !codexSafeOverride(args[i]) {
				return a + " " + overrideKey(args, i), true
			}
		case strings.HasPrefix(a, "--config="):
			if !codexSafeOverride(strings.TrimPrefix(a, "--config=")) {
				return "--config " + overrideKey([]string{strings.TrimPrefix(a, "--config=")}, 0), true
			}
		default:
			return flagName(a), true
		}
	}
	return "", false
}

// codexSafeOverrides are the -c keys that cannot widen codex's sandbox, with
// the check each value must pass. sandbox_mode may only name a mode that keeps
// the sandbox on.
var codexSafeOverrides = map[string]func(v string) bool{
	"model":                   func(string) bool { return true },
	"model_reasoning_effort":  func(string) bool { return true },
	"model_reasoning_summary": func(string) bool { return true },
	"approval_policy": func(v string) bool {
		return v == "untrusted" || v == "on-failure" || v == "on-request" || v == "never"
	},
	"sandbox_mode": func(v string) bool { return v == "workspace-write" || v == "read-only" },
}

func codexSafeOverride(kv string) bool {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return false
	}
	check, known := codexSafeOverrides[strings.TrimSpace(k)]
	return known && check(strings.Trim(strings.TrimSpace(v), `"'`))
}

// overrideKey is the key of the override at args[i], or "?" when there is none:
// enough to name the flag in a log without echoing a value.
func overrideKey(args []string, i int) string {
	if i >= len(args) {
		return "?"
	}
	k, _, _ := strings.Cut(args[i], "=")
	return flagName(k)
}

// flagName is arg cut at its first "=" and bounded, so a log line names a
// flag without carrying its value.
func flagName(arg string) string {
	name, _, _ := strings.Cut(arg, "=")
	if len(name) > 48 {
		name = name[:48] + "…"
	}
	return name
}

// codexEnvUnsafe returns why the launch's environment rules out the
// exemption: a CODEX_* variable other than CODEX_HOME (CODEX_EXEC_SERVER_URL
// and the like change what codex runs and where), or a CODEX_HOME that is not
// the session's own boot dir under workspaceDir.
func codexEnvUnsafe(env []string, workspaceDir string) string {
	home := ""
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "CODEX_HOME":
			home = v
		case strings.HasPrefix(k, "CODEX_"):
			return "the environment sets " + k
		}
	}
	// codex resolves a relative TMPDIR against its working directory, so the
	// directory it makes writable is one this check cannot name.
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok && v != "" && !filepath.IsAbs(v) {
			return "TMPDIR is relative, so the directory codex makes writable is not one Tether can check"
		}
	}
	if home == "" || workspaceDir == "" {
		return "CODEX_HOME is not set to the session's boot dir"
	}
	if !pathWithin(realPathOrClean(workspaceDir), realPathOrClean(home)) {
		return "CODEX_HOME is outside the session workspace"
	}
	return ""
}

// codexProjectConfigPresent returns why a project-level .codex/config.toml in
// dir, or any directory above it up to the project root, rules out the
// exemption: codex layers it over its configuration, writable_roots included
// (real codex 0.159.x honors the one in its working directory). The walk
// stops at the project root, the nearest directory holding a .git entry, and
// never reaches the user's home directory, whose ~/.codex/config.toml is the
// user's own configuration, not a project's.
func codexProjectConfigPresent(dir string) string {
	home, _ := os.UserHomeDir()
	home = realPathOrClean(home)
	dir = realPathOrClean(dir)
	for {
		if dir == home && home != "" {
			return ""
		}
		cfg := filepath.Join(dir, ".codex", "config.toml")
		if _, err := os.Stat(cfg); err == nil {
			return fmt.Sprintf("%s exists, and codex layers a project config over its sandbox settings", cfg)
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// codexSandboxWeakened reports whether catalog flags turn off or widen
// codex's sandbox: the bypass flags, or a sandbox_mode of danger-full-access
// given by --sandbox/-s or a --config/-c override. The allowlist already
// refuses these; this names the common cases first.
func codexSandboxWeakened(args []string) bool {
	for i, a := range args {
		switch {
		case a == "--dangerously-bypass-approvals-and-sandbox", a == "--yolo":
			return true
		case a == "--sandbox" || a == "-s":
			if i+1 < len(args) && args[i+1] == "danger-full-access" {
				return true
			}
		case a == "--sandbox=danger-full-access":
			return true
		case a == "--config" || a == "-c":
			if i+1 < len(args) && isFullAccessOverride(args[i+1]) {
				return true
			}
		case strings.HasPrefix(a, "--config="):
			if isFullAccessOverride(strings.TrimPrefix(a, "--config=")) {
				return true
			}
		}
	}
	return false
}

// isFullAccessOverride matches a -c override of sandbox_mode to
// danger-full-access, however it is quoted.
func isFullAccessOverride(kv string) bool {
	k, v, ok := strings.Cut(kv, "=")
	if !ok || strings.TrimSpace(k) != "sandbox_mode" {
		return false
	}
	return strings.Trim(strings.TrimSpace(v), `"'`) == "danger-full-access"
}

// codexHomeFromEnv is CODEX_HOME in an environment, or "".
func codexHomeFromEnv(env []string) string {
	home := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "CODEX_HOME="); ok {
			home = v
		}
	}
	return home
}

// codexWritableRoots are the directories codex's workspace-write sandbox lets
// the agent write besides what a turn creates: the project's work root and
// repo root (its --cd), /tmp and $TMPDIR.
func codexWritableRoots(plan *launch.Plan, env []string) []string {
	var roots []string
	if plan != nil {
		for _, r := range []string{plan.EffectiveWorkRoot(), plan.RepoRoot} {
			if r != "" && !containsPath(roots, r) {
				roots = append(roots, r)
			}
		}
	}
	return append(roots, codexExtraWritableRoots(env)...)
}

// The shape of the config.toml Tether plants into CODEX_HOME
// (go-providers' codex boot dir): approval_policy and sandbox_mode at the top,
// [mcp_servers.<name>] tables, and the [projects."<path>"] trust_level entry
// codex adds itself. No TOML library is in the module graph, and the shape is
// closed and small, so a strict line check stands in for a parser. It fails
// closed: a line it does not recognize is not known-safe. A real parser would
// accept more, never less, so this can only over-refuse.
//
// MAINTENANCE: if a newer codex writes other keys into this file at runtime,
// exempted sessions are refused with a 403 naming the file and line, which is
// loud and fail-closed. The fix is to add the key here once it is known not to
// widen the sandbox. Today codex adds only [projects."<path>"] trust_level.
var (
	codexTopLine       = regexp.MustCompile(`^(approval_policy|sandbox_mode)\s*=\s*"([^"]*)"\s*(#.*)?$`)
	codexMCPHeader     = regexp.MustCompile(`^\[mcp_servers\.[A-Za-z0-9_."-]+\]\s*(#.*)?$`)
	codexProjectHeader = regexp.MustCompile(`^\[projects\."[^"\]]*"\]\s*(#.*)?$`)
	codexTrustLine     = regexp.MustCompile(`^trust_level\s*=\s*"[a-z_]*"\s*(#.*)?$`)
)

// codexConfigUnsafe returns why the config.toml at path is not known to leave
// codex's sandbox as Tether planted it, or "" when it is, or absent. It names
// the line and the key, never a value.
func codexConfigUnsafe(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the session's own CODEX_HOME
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("%s cannot be read to check it: %v", path, err)
	}
	section := "" // "" at the top, then "mcp" or "project"
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			switch {
			case codexMCPHeader.MatchString(line):
				section = "mcp"
			case codexProjectHeader.MatchString(line):
				section = "project"
			default:
				return fmt.Sprintf("%s:%d opens the table %s, which is not one Tether planted or codex adds", path, i+1, flagName(strings.TrimSpace(strings.Split(line, "]")[0])))
			}
			continue
		}
		switch section {
		case "mcp":
			// An MCP server's own definition: command, args, env.
		case "project":
			if !codexTrustLine.MatchString(line) {
				return fmt.Sprintf("%s:%d sets %s in a project table, where codex adds only trust_level", path, i+1, flagName(line))
			}
		default:
			m := codexTopLine.FindStringSubmatch(line)
			if m == nil {
				return fmt.Sprintf("%s:%d sets %s, which is not a key Tether plants", path, i+1, flagName(line))
			}
			if m[1] == "sandbox_mode" && m[2] != "workspace-write" && m[2] != "read-only" {
				return fmt.Sprintf("%s:%d sets sandbox_mode to a mode that switches codex's sandbox off", path, i+1)
			}
			if m[1] == "approval_policy" && !map[string]bool{"untrusted": true, "on-failure": true, "on-request": true, "never": true}[m[2]] {
				return fmt.Sprintf("%s:%d sets approval_policy to a value Tether does not know", path, i+1)
			}
		}
	}
	return ""
}

// codexExemption is what a launch that Tether left to codex's own sandbox
// needs re-checked before each turn. The exemption was judged at launch from
// what was on disk then; codex's sandbox protects .codex only from codex, so
// another agent sharing the project directory, a git checkout or an operator
// can plant a .codex/config.toml (or change CODEX_HOME/config.toml) between
// turns, and codex exec reads it afresh each turn.
type codexExemption struct {
	workDirs  []string // the launch's work directories, where codex looks for .codex/config.toml
	home      string   // CODEX_HOME, whose config.toml codex reads
	protected []string // the directories Tether protects, which no work directory may contain
}

// widened returns why the exemption no longer holds, or "". It resolves every
// path afresh, so a work root that is a symlink retargeted since the launch is
// judged by where it points now.
func (e *codexExemption) widened() string {
	for _, d := range e.workDirs {
		resolved := realPathOrClean(d)
		for _, p := range e.protected {
			if pathWithin(resolved, p) || pathWithin(p, resolved) {
				return fmt.Sprintf("the work directory %s now resolves to %s, which overlaps the protected directory %s", d, resolved, p)
			}
		}
		if why := codexProjectConfigPresent(d); why != "" {
			return why
		}
	}
	if e.home != "" {
		return codexConfigUnsafe(filepath.Join(e.home, "config.toml"))
	}
	return ""
}

// codexSafeMCPUpstreams are the only MCP upstreams a guarded (and so exempted)
// codex agent may be granted. Dormant with the rest of the guard: codex ships
// as CodexNotProtected (see codexProtectionMode).
//
// Codex spawns the MCP servers it is configured with itself, outside its
// sandbox, so the planted proxy and every upstream it starts run unsandboxed:
// whatever tools an upstream has are tools the agent has, with the operator's
// uid and none of codex's confinement. This list screens out upstreams with a
// host-exec or arbitrary file-write tool, and it is NOT sufficient: torque and
// tesseract are the default allow-list, yet `torque mcp` boots agents
// in-process and torque_session_launch takes a caller-chosen workdir
// (CW-20261001-0464), so a list of upstreams cannot make an exempted codex
// sound. The real fix is upstreams that run daemon-side (CW-20261001-0230).
//
// EXTEND ONLY AFTER VERIFYING the upstream has no host-exec or arbitrary
// file-write tools, since codex spawns MCP children unsandboxed. It is a
// constant of its own and not launch.DefaultMCPServers on purpose: changing the
// default must not silently widen what runs outside a sandbox.
var codexSafeMCPUpstreams = []string{"torque", "tesseract"}

// unsafeMCPUpstream returns the first upstream on the launch's effective MCP
// allow-list that is not in codexSafeMCPUpstreams, or "".
func unsafeMCPUpstream(plan *launch.Plan) string {
	for _, id := range launch.EffectiveMCPServers(plan.Env) {
		safe := false
		for _, ok := range codexSafeMCPUpstreams {
			if strings.EqualFold(id, ok) {
				safe = true
				break
			}
		}
		if !safe {
			return id
		}
	}
	return ""
}
