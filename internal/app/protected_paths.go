package app

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hollis-labs/agentkit/agentsessions"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/acp"
)

// ProtectEnv names the daemon environment variable that turns control-plane
// protection off: "0" or "false" in tetherd's environment. Protection is on by
// default; turning it off is an operator decision the daemon logs at startup
// and `tether doctor` reports, never a silent one.
const ProtectEnv = "TETHER_SANDBOX_PROTECT"

// ProtectionStatus says whether the agents Tether launches get its own
// directories as ProtectedPaths, and why.
type ProtectionStatus struct {
	Enabled bool
	// DisabledByOperator is set when ProtectEnv turned protection off.
	DisabledByOperator bool
	// Reason says what the status means for an agent, for logs and doctor.
	Reason string
}

// ControlPlaneProtection decides control-plane protection for an OS and a
// daemon environment (CW-20261001-0142). It applies on Linux only for now:
// go-sandbox's seatbelt protection on macOS is unverified on a real Mac
// (CW-20261001-0138) and fails closed, so darwin launches stay unprotected
// until that is verified rather than risk refusing every launch there.
func ControlPlaneProtection(goos string, getenv func(string) string) ProtectionStatus {
	raw := getenv(ProtectEnv)
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false":
		return ProtectionStatus{DisabledByOperator: true, Reason: fmt.Sprintf("DISABLED by %s=%s, so agents can write the catalog, run/ and state/", ProtectEnv, raw)}
	}
	if goos != "linux" {
		return ProtectionStatus{Reason: fmt.Sprintf("not applied on %s until go-sandbox's seatbelt protection is verified on a real Mac (CW-20261001-0138), so agents can write the catalog, run/ and state/", goos)}
	}
	if codexProtectionMode == CodexNotProtected {
		return ProtectionStatus{Enabled: true, Reason: "on: Claude, OpenCode and every agent Tether wraps cannot write the catalog, run/ or state/; Codex is NOT protected (CW-20261001-0230), its provider sandbox is disabled for bypass sessions"}
	}
	return ProtectionStatus{Enabled: true, Reason: "on: agents Tether wraps cannot write the catalog, run/ or state/; Codex is left to its own workspace-write sandbox under Tether's dormant guard, and its MCP servers run outside that sandbox (CW-20261001-0230)"}
}

// defaultProtectionStatus is the daemon's protection decision, taken from
// its OS and environment. Only this package's TestMain replaces it, once
// before any test runs, so unit tests that launch a stand-in CLI do not need
// bubblewrap; a test of protection sets Service.protectionStatus instead.
var defaultProtectionStatus = func() ProtectionStatus {
	return ControlPlaneProtection(runtime.GOOS, os.Getenv)
}

// bwrapAvailable reports whether this host can build the sandbox protection
// uses, around dir: bubblewrap on PATH that can create the namespaces. A
// variable so the refusal can be tested.
var bwrapAvailable = func(dir string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	return cachedBwrapProbe(dir)
}

func (s *Service) protectsControlPlane() bool {
	if s.protectionStatus != nil {
		return s.protectionStatus().Enabled
	}
	return defaultProtectionStatus().Enabled
}

// protectionPlan decides what protecting one launch means: the directories
// to protect, and whether Tether's own sandbox must wrap the agent to do it.
// A nil error with no directories means the launch needs nothing (protection
// off, or the in-process API stub). It refuses an ACP runtime, which cannot
// be protected until CW-20261001-0162: go-agent-wrapper's ACP launcher
// refuses ProtectedPaths without a resolved sandbox policy, and running the
// agent unprotected instead would be fail-open.
//
// Codex is the one runtime that may not need Tether's sandbox: it runs inside
// its own workspace-write sandbox, which is bubblewrap too, and bubblewrap
// cannot nest where unprivileged user namespaces are restricted (AppArmor's
// kernel.apparmor_restrict_unprivileged_userns=1, the Ubuntu default). Its
// sandbox makes the whole filesystem read-only except its writable roots,
// which exclude the protected directories. That holds only while everything
// that shapes the sandbox is known-safe; see codexOwnsSandbox, an allowlist.
// opts is nil at session create and resume, where only the plan can be judged.
func (s *Service) protectionPlan(plan *launch.Plan, kind string, opts *agentsessions.StartOptions, announce bool) (dirs []string, outer bool, err error) {
	if kind == "api" || !s.protectsControlPlane() {
		return nil, false, nil
	}
	if kind == acp.Kind {
		return nil, false, launch.ErrACPLaunchUnprotected
	}
	if plan != nil && plan.ProviderBrand == "codex" && codexProtectionMode == CodexNotProtected {
		// The fallback: codex runs as it did before protection existed, with
		// none of the guard (see codexProtectionMode).
		return nil, false, nil
	}
	dirs, err = s.controlPlaneDirsFor(planProject(plan))
	if err != nil {
		// The project this launch is for has no usable repo_root: say so, typed,
		// rather than as a bare internal error.
		var rootErr *config.ProjectRootError
		if errors.As(err, &rootErr) {
			return nil, false, fmt.Errorf("%w: %w", launch.ErrLaunchProjectRootMissing, err)
		}
		return nil, false, err
	}
	if plan != nil && plan.ProviderBrand == "codex" {
		ok, why := codexOwnsSandbox(plan, opts, dirs)
		if ok {
			return dirs, false, nil
		}
		if announce && opts != nil {
			// Wrapping codex is the fail-closed outcome, and where the
			// sandbox cannot nest it fails loudly: say why it is happening.
			log.Printf("app: codex launch %s is wrapped in Tether's sandbox instead of relying on codex's own: %s", plan.LaunchID, why)
		}
	}
	return dirs, true, nil
}

// refuseUnprotectable refuses, while protection is on, a launch that could
// not be protected: an ACP runtime, and any runtime Tether must sandbox on a
// host where bubblewrap is missing or cannot build a namespace. Session
// create, resume and launch all call it, so a refused launch fails before it
// starts, with the reason.
func (s *Service) refuseUnprotectable(plan *launch.Plan, kind string) error {
	dirs, outer, err := s.protectionPlan(plan, kind, nil, false)
	if err != nil || !outer {
		return err
	}
	return protectionUnavailable(dirs)
}

// protectionUnavailable is nil when this host can sandbox a launch around
// the first protected directory, and otherwise the refusal.
func protectionUnavailable(dirs []string) error {
	if len(dirs) == 0 {
		return nil
	}
	if err := bwrapAvailable(dirs[0]); err != nil {
		return fmt.Errorf("%w: %w: install bubblewrap and allow unprivileged user namespaces, or set %s=0 in tetherd's environment to run agents unprotected", launch.ErrProtectionUnavailable, err, ProtectEnv)
	}
	return nil
}

// applyControlPlaneProtection sets opts.ProtectedPaths to the directories no
// agent may write: the catalog root, the daemon's run directory (pid file
// and control socket) when it lies inside Tether's root, and the directory
// holding the state database. Each is registered by its real path, as
// go-sandbox requires. A launch whose work directory or workspace lies inside
// one of them is refused with launch.ErrLaunchInsideProtectedPath: the agent
// could not work there. A codex launch is refused the same way even though
// Tether does not wrap it (see protectionPlan): its own sandbox would let it
// write its work directory.
//
// The state directory can be protected because the `tether mcp` server planted
// in each agent runs daemon-only (CW-20261001-0173): it never opens the
// database, and reads and writes Tether's state through the daemon.
//
// The in-process API stub starts no agent process, so there is nothing to
// protect it from.
func (s *Service) applyControlPlaneProtection(plan *launch.Plan, kind string, opts *agentsessions.StartOptions) error {
	dirs, outer, err := s.protectionPlan(plan, kind, opts, true)
	if err != nil || len(dirs) == 0 {
		return err
	}
	writable := launchDirs(plan, opts)
	for _, w := range writable {
		resolved := realPathOrClean(w.path)
		for _, dir := range dirs {
			if pathWithin(dir, resolved) {
				return fmt.Errorf("%w: the agent's %s %s is inside %s, which Tether write-protects for every agent; move it out of that directory", launch.ErrLaunchInsideProtectedPath, w.what, w.path, dir)
			}
		}
	}
	if !outer {
		return nil
	}
	if err := protectionUnavailable(dirs); err != nil {
		return err
	}
	opts.ProtectedPaths = dirs
	return nil
}

// controlPlaneDirs returns the real paths of the directories to protect.
// The catalog root must resolve: protection that cannot name the catalog
// fails closed. The run directories (the daemon's pid file and unix socket)
// are protected when they exist and lie inside a Tether root: the catalog
// root's parent, or the default ~/.tether, whichever the run directory is
// in. A pid file configured into a shared directory such as /tmp is skipped:
// it must not make that directory read-only for every agent.
//
// The state database's directory is always protected, wherever it is: it
// holds every session, message and event, and the WAL and shared-memory
// files beside the database, which an agent could otherwise replace. If it
// does not exist the daemon has not created it, so there is nothing to
// protect. A directory that also holds an agent's work directory or
// workspace refuses that launch (applyControlPlaneProtection) rather than
// leaving the state database writable.
//
// Protecting it wherever it is has a consequence the run directory does not:
// a defaults.state_db in a shared directory ($HOME, /tmp, /) makes that whole
// directory read-only for every wrapped agent. That fails closed and visibly
// (writes there fail, and a launch whose work directory or workspace is
// inside it is refused), but it is a footgun: the state database belongs in a
// directory of its own, as the seeded ~/.tether/state/ is.
func (s *Service) controlPlaneDirs() ([]string, error) { return s.controlPlaneDirsFor("") }

// planProject is the project a launch plan is for, or "" when there is no plan.
func planProject(plan *launch.Plan) string {
	if plan == nil {
		return ""
	}
	return plan.ProjectID
}

// controlPlaneDirsFor is controlPlaneDirs for a launch of project launching: if
// that project's repo_root is unusable the launch cannot be protected and a
// *config.ProjectRootError says which project and path. Any other project with
// an unusable repo_root is left out (and reported, see controlPlane).
func (s *Service) controlPlaneDirsFor(launching string) ([]string, error) {
	dirs, _, err := s.controlPlane(launching)
	return dirs, err
}

// controlPlane returns what controlPlaneDirsFor does, plus the registered
// projects whose layer it had to leave out because their repo_root cannot be
// used. Each such project is warned about once per daemon, and a layer it had
// to create is logged, so neither the skip nor the side effect is silent.
func (s *Service) controlPlane(launching string) ([]string, []config.SkippedProjectLayer, error) {
	catalogRoot := config.Expand(s.CatalogRoot)
	if catalogRoot == "" {
		return nil, nil, errors.New("protect control plane: the catalog root is not set")
	}
	catalog, err := realDir(catalogRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("protect control plane: catalog root: %w", err)
	}
	prepared, err := config.PrepareCatalogProtection(catalog, s.Catalog, launching)
	if err != nil {
		return nil, nil, err
	}
	s.reportProtectionLayers(prepared)
	dirs := prepared.Dirs
	if s.Catalog == nil {
		return dirs, prepared.Skipped, nil
	}
	roots := []string{filepath.Dir(catalog)}
	if home, err := os.UserHomeDir(); err == nil {
		if tetherHome, err := realDir(filepath.Join(home, ".tether")); err == nil && !containsPath(roots, tetherHome) {
			roots = append(roots, tetherHome)
		}
	}
	for _, candidate := range runDirs(s.Catalog.Global.Daemon) {
		dir, err := realDir(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("protect control plane: run directory: %w", err)
		}
		if containsPath(dirs, dir) || !insideAnyRoot(roots, dir) {
			continue
		}
		dirs = append(dirs, dir)
	}
	if db := config.ResolveStateDB(s.Catalog.Global.Catalog.Defaults, s.Catalog.Paths); db != "" {
		dir, err := realDir(filepath.Dir(config.Expand(db)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, nil, fmt.Errorf("protect control plane: state directory: %w", err)
		case !containsPath(dirs, dir):
			dirs = append(dirs, dir)
		}
	}
	return dirs, prepared.Skipped, nil
}

// reportProtectionLayers makes what protection did to project layers visible.
// A project left out because its repo_root is unusable is warned about once per
// daemon (the warning says what that leaves open); an empty layer protection had
// to create is logged, since that writes into a project's repository.
func (s *Service) reportProtectionLayers(p config.CatalogProtection) {
	for _, skipped := range p.Skipped {
		if _, seen := s.protectionWarned.LoadOrStore(skipped.Project+"\x00"+skipped.RepoRoot, struct{}{}); seen {
			continue
		}
		log.Printf("WARN: protect: project %q is left out of control-plane protection: its repo_root %s %s. A protected agent could create that directory and a .tether layer in it, which the next catalog load would read for this project: fix or remove the project", skipped.Project, skipped.RepoRoot, skipped.Reason)
	}
	for _, created := range p.Created {
		log.Printf("protect: created the empty layer directory %s so that no agent can plant one there", created)
	}
}

// insideAnyRoot reports whether dir lies strictly beneath one of roots.
func insideAnyRoot(roots []string, dir string) bool {
	for _, root := range roots {
		if dir != root && pathWithin(root, dir) {
			return true
		}
	}
	return false
}

// runDirs names the directories holding the daemon's pid file and, for a
// unix listen address, its control socket.
func runDirs(d config.DaemonConfig) []string {
	var out []string
	if d.PIDFile != "" {
		out = append(out, filepath.Dir(config.Expand(d.PIDFile)))
	}
	if sock, ok := strings.CutPrefix(d.ListenAddr, "unix:"); ok && sock != "" {
		out = append(out, filepath.Dir(config.Expand(sock)))
	}
	return out
}

// realDir resolves path through any symlinks and requires a directory.
func realDir(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return resolved, nil
}

// realPathOrClean resolves path through symlinks, the existing prefix of a
// path that does not exist yet included (config.RealPath), so a containment
// check compares like with like.
func realPathOrClean(path string) string { return config.RealPath(path) }

// pathWithin reports whether path is dir or lies beneath it.
func pathWithin(dir, path string) bool { return config.PathWithin(dir, path) }

func containsPath(paths []string, path string) bool {
	for _, p := range paths {
		if p == path {
			return true
		}
	}
	return false
}

// codexExemptionFor returns what to re-check before each turn when a launch is
// left to codex's own sandbox, and nil when it is not (another provider,
// protection off, or Tether wraps it). It runs after applyControlPlaneProtection
// accepted the launch, and re-asks the same question without logging.
func (s *Service) codexExemptionFor(plan *launch.Plan, kind string, opts *agentsessions.StartOptions) *codexExemption {
	if plan == nil || plan.ProviderBrand != "codex" || codexProtectionMode != CodexGuarded {
		return nil
	}
	dirs, outer, err := s.protectionPlan(plan, kind, opts, false)
	if err != nil || outer || len(dirs) == 0 {
		return nil
	}
	ex := &codexExemption{home: codexHomeFromEnv(opts.Env), protected: dirs}
	for _, d := range launchDirs(plan, opts) {
		if d.what != "workspace" {
			ex.workDirs = append(ex.workDirs, d.path)
		}
	}
	return ex
}

// refuseWidenedCodex refuses a turn on a codex session Tether left to its own
// sandbox when something that shapes that sandbox has since changed: a project
// .codex/config.toml in a work directory, or a CODEX_HOME config.toml no longer
// of the shape Tether planted. Every way a turn is delivered (SendTurn,
// SendInput, and so the app-server turn/start, the wake sweep and the MCP and
// HTTP routes) calls it first. A session that was wrapped, or never exempt,
// is not in the registry and passes.
//
// It narrows a window, it does not close it: the file could appear between this
// check and codex reading its configuration, which takes a co-located writer.
// What closes it is caller identity (CW-20260930-0253) plus a way to make .codex
// unwritable to other agents.
func (s *Service) refuseWidenedCodex(sessionID string) error {
	v, ok := s.codexExempt.Load(sessionID)
	if !ok {
		return nil
	}
	why := v.(*codexExemption).widened()
	if why == "" {
		return nil
	}
	log.Printf("app: refusing a turn on session %s: %s", sessionID, why)
	return fmt.Errorf("%w: %s; remove it, or relaunch the session so Tether wraps the agent instead of relying on codex's own sandbox", launch.ErrCodexSandboxWidened, why)
}

// mcpProtectedPaths are the directories the planted `tether mcp` must refuse to
// write: the ones protection registers for the agent, or none while
// protection is off. An agent that is not protected (the kill switch, darwin)
// gets no protect-path either, so the server behaves as it did.
//
// The proxy is now confined even while the Codex agent guard is dormant, so
// failure to name the directories fails every protected launch closed.
func (s *Service) mcpProtectedPaths(plan *launch.Plan) ([]string, error) {
	if !s.protectsControlPlane() {
		return nil, nil
	}
	return s.controlPlaneDirsFor(planProject(plan))
}
