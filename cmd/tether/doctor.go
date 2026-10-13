package main

// doctor.go — tether detect + tether doctor: provider detection table and health checks (T-v06x-01-04).
//
// tether detect: print provider detection results; --json for machine output. Exit 0 always.
// tether doctor: run ordered health checks and exit non-zero if any check is fail.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/catalogcheck"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/setup"
	"github.com/hollis-labs/tether/internal/store"
)

// ── tether detect ───────────────────────────────────────────────────────────────

var detectCmd = &cobra.Command{
	Use:   "detect",
	Short: "Show which CLI providers (claude, codex, opencode, antigravity) were found on this system",
	Long: `tether detect calls DetectProviders() and prints a table showing which CLI
providers were located via environment variables, PATH, or well-known install
paths. It never modifies any files.

Exit code is always 0 — the command is purely informational.

--json prints a JSON array for scripting.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOut, _ := cmd.Flags().GetBool("json")
		return runDetect(os.Stdout, jsonOut) //nolint:wrapcheck
	},
}

func init() {
	detectCmd.Flags().Bool("json", false, "print JSON array instead of a table")
}

// detectRow is the stable JSON shape for tether detect --json.
type detectRow struct {
	Brand  string `json:"brand"`
	Found  bool   `json:"found"`
	Path   string `json:"path,omitempty"`
	Source string `json:"source"`
}

func runDetect(out io.Writer, jsonOut bool) error {
	results := setup.DetectProviders()

	if jsonOut {
		rows := make([]detectRow, len(results))
		for i, r := range results {
			rows[i] = detectRow{Brand: r.Brand, Found: r.Found, Path: r.Path, Source: r.Source}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	fmt.Fprintf(out, "%-12s  %-6s  %-8s  %s\n", "BRAND", "FOUND", "SOURCE", "PATH")
	fmt.Fprintf(out, "%-12s  %-6s  %-8s  %s\n", "-----", "-----", "------", "----")
	for _, r := range results {
		found := "no"
		if r.Found {
			found = "yes"
		}
		fmt.Fprintf(out, "%-12s  %-6s  %-8s  %s\n", r.Brand, found, r.Source, r.Path)
	}
	return nil
}

// ── tether doctor ───────────────────────────────────────────────────────────────

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check Tether install health: daemon, catalog, migrations, providers, and paths",
	Long: `tether doctor runs an ordered set of checks against your Tether installation
and reports ok / warn / fail for each one, with a one-line remedy on failures.

Exit code:
  0 — all checks ok or warn (warnings are advisory, not blocking)
  1 — one or more checks failed

--json prints a JSON array of check results for scripting.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOut, _ := cmd.Flags().GetBool("json")
		stateDir := filepath.Dir(config.Expand(catalogPath))
		live, _ := cmd.Flags().GetBool("mcp-live")
		return runDoctor(os.Stdout, stateDir, catalogPath, jsonOut, live) //nolint:wrapcheck
	},
}

func init() {
	doctorCmd.Flags().Bool("json", false, "print JSON array of check results")
	doctorCmd.Flags().Bool("mcp-live", false, "spawn configured MCP upstreams for a bounded initialize/tools/list-only name/metadata probe (no tool calls; may resolve helper credentials)")
	doctorCmd.Flags().StringArrayVar(&doctorProtect, "protect-path", nil, "protected paths for live MCP probe confinement (repeatable, same as tether mcp)")
}

// checkResult is the stable JSON shape for tether doctor --json.
type checkResult = catalogcheck.Result

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

func ok(name, msg string) checkResult { return catalogcheck.OK(name, msg) }
func warn(name, msg, remedy string) checkResult {
	return catalogcheck.Warn(name, msg, remedy)
}
func fail(name, msg, remedy string) checkResult {
	return catalogcheck.Fail(name, msg, remedy)
}

var doctorProtect []string

func runDoctor(out io.Writer, stateDir, catalogRoot string, jsonOut bool, live ...bool) error {
	var checks []checkResult

	// 1. State dir exists + writable.
	checks = append(checks, checkStateDir(stateDir))

	// 2. Catalog present + valid.
	var cat *config.Catalog
	catalogCheck, loadedCat := checkCatalog(catalogRoot)
	checks = append(checks, catalogCheck)
	cat = loadedCat
	checks = append(checks, checkShimHosting(cat)...)
	if cat != nil {
		checks = append(checks, catalogcheck.RoleModules(cat)...)
		checks = append(checks, catalogcheck.SandboxProfiles(cat))
		checks = append(checks, catalogcheck.MCPDiscoveryMode(cat), catalogcheck.MCPEndpoint(cat))
		checks = append(checks, catalogcheck.MCPProfiles(cat, catalogRoot)...)
		checks = append(checks, catalogcheck.MCPNamingConfig(catalogRoot)...)
		if len(live) > 0 && live[0] {
			checks = append(checks, checkMCPLiveNames(cat, catalogRoot)...)
		}
		checks = append(checks, ok("events-retention", retentionMessage(cat.Global.Daemon.EventsRetention)))
	}
	checks = append(checks, doctorSandboxProtect(cat, catalogRoot)...)
	checks = append(checks, catalogcheck.MCPCredentialFiles(catalogRoot)...)

	// 3. Daemon reachable (requires catalog for listen addr).
	checks = append(checks, checkDaemon(cat), checkIdentity(cat), checkMCPUpstreamOwnership(cat))
	checks = append(checks, checkClaudeStrictMCP(cat, localStrictMCPStatus()))

	// 4. Migrations current (opens DB; idempotent — migrations are a no-op if already applied).
	checks = append(checks, checkMigrations(cat))

	// 5. Provider commands resolvable.
	checks = append(checks, checkProviders(cat)...)

	// 5b. Provider logins that a launch must not discover by itself.
	checks = append(checks, checkProviderAuth(cat)...)

	// 6. Logs dir exists + writable.
	checks = append(checks, checkLogsDir(stateDir))

	// ── output ────────────────────────────────────────────────────────────
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(checks); err != nil {
			return err
		}
	} else {
		for _, c := range checks {
			var icon string
			switch c.Status {
			case statusWarn:
				icon = "⚠"
			case statusFail:
				icon = "✗"
			default:
				icon = "✓"
			}
			fmt.Fprintf(out, "%s  %-32s %s\n", icon, c.Name, c.Message)
			if c.Remedy != "" {
				fmt.Fprintf(out, "   %s\n", c.Remedy)
			}
		}
	}

	for _, c := range checks {
		if c.Status == statusFail {
			return fmt.Errorf("one or more checks failed")
		}
	}
	return nil
}

// checkStateDir verifies the state root directory exists and is writable.
func checkStateDir(stateDir string) checkResult {
	info, err := os.Stat(stateDir)
	if err != nil {
		return fail("state-dir", fmt.Sprintf("not found: %s", stateDir),
			"run: tether init")
	}
	if !info.IsDir() {
		return fail("state-dir", fmt.Sprintf("not a directory: %s", stateDir),
			"remove the conflicting file and run: tether init")
	}
	// Writable check: attempt temp-file creation.
	f, err := os.CreateTemp(stateDir, ".doctor-write-check.*")
	if err != nil {
		return warn("state-dir", fmt.Sprintf("directory not writable: %s", stateDir),
			fmt.Sprintf("fix permissions: chmod u+w %s", stateDir))
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return ok("state-dir", stateDir)
}

// checkCatalog loads and validates the catalog. Returns the loaded catalog (nil on failure).
func checkCatalog(catalogRoot string) (checkResult, *config.Catalog) {
	cat, res := catalogcheck.LoadAndValidate(catalogRoot)
	return res, cat
}

// checkDaemon pings the daemon. Requires a loaded catalog for the listen address.
func checkDaemon(cat *config.Catalog) checkResult {
	if cat == nil {
		return warn("daemon-reachable", "skipped — catalog unavailable", "fix catalog first")
	}
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil {
		return warn("daemon-reachable", fmt.Sprintf("cannot resolve listen addr: %v", err), "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c := daemonClient(cfg.ListenAddr)
	if err := c.Ping(ctx); err != nil {
		return warn("daemon-reachable", "daemon not running",
			"start it with: tether daemon start")
	}
	return ok("daemon-reachable", cfg.ListenAddr)
}

// checkMigrations opens the SQLite store (which applies pending migrations) and closes it.
func checkMigrations(cat *config.Catalog) checkResult {
	if cat == nil {
		return warn("migrations-current", "skipped — catalog unavailable", "fix catalog first")
	}
	dbPath := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
	if dbPath == "" {
		return fail("migrations-current", "state_db path not configured",
			"add catalog.defaults.state_db to global.yaml")
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return fail("migrations-current", fmt.Sprintf("open/migrate failed: %v", err),
			"run: tether init  or check DB permissions at "+dbPath)
	}
	_ = db.Close()
	return ok("migrations-current", dbPath)
}

// checkProviderAuth reports on providers whose CLI would otherwise fall into
// an interactive login. For agy that is a browser sign-in, which a launch
// must never trigger. agy keeps its login in the system keychain, which no
// static check can read without risking a keychain prompt, so go-providers
// v0.30.0 dropped the old ~/.gemini/oauth_creds.json stat (that file belongs
// to the retired Gemini CLI, so the check was wrong both ways). The check now
// says what it cannot see; a launch that is not signed in fails as not
// authenticated after the fact.
func checkProviderAuth(cat *config.Catalog) []checkResult {
	if cat == nil {
		return nil
	}
	ids := make([]string, 0, len(cat.Providers))
	for id, p := range cat.Providers {
		if p.ProviderBrand() == "antigravity" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	results := make([]checkResult, 0, len(ids))
	for _, id := range ids {
		results = append(results, warn("provider-auth:"+id,
			"agy sign-in not checked: agy keeps its login in the system keychain, which cannot be read here",
			"if an agy launch fails as not authenticated, run `agy` interactively once to sign in"))
	}
	return results
}

// checkProviders checks that each configured provider's command is resolvable.
func checkProviders(cat *config.Catalog) []checkResult {
	if cat == nil {
		return []checkResult{warn("providers", "skipped — catalog unavailable", "fix catalog first")}
	}
	if len(cat.Providers) == 0 {
		return []checkResult{warn("providers", "no providers configured",
			"run: tether init  to seed provider catalog")}
	}

	detected := setup.DetectProviders()
	detectedByBrand := make(map[string]setup.DetectResult, len(detected))
	for _, d := range detected {
		detectedByBrand[d.Brand] = d
	}

	var results []checkResult
	for id, p := range cat.Providers {
		name := "provider:" + id
		cmd := p.Command
		if cmd == "" {
			// Empty command: rely on adapter detect. Warn if not detected.
			brand := p.ProviderBrand() // canonical runtime id, as detection reports it
			if d, found := detectedByBrand[brand]; found && d.Found {
				results = append(results, ok(name, "auto-detect → "+d.Path))
			} else {
				results = append(results, warn(name,
					"command empty and binary not auto-detected",
					fmt.Sprintf("set command in catalog/providers/%s.yaml or install the CLI", id)))
			}
			continue
		}
		// Non-empty command: resolve it the way the runtime execs it — a bare
		// name (claude, codex, npx) is looked up on $PATH; an absolute/relative
		// path is stat'd and checked for the executable bit.
		if !strings.ContainsRune(cmd, os.PathSeparator) {
			resolved, err := exec.LookPath(cmd)
			if err != nil {
				results = append(results, fail(name,
					fmt.Sprintf("command not found on PATH: %s", cmd),
					fmt.Sprintf("install the CLI or set an absolute command in catalog/providers/%s.yaml", id)))
				continue
			}
			results = append(results, ok(name, resolved))
			continue
		}
		expanded := config.Expand(cmd)
		info, err := os.Stat(expanded)
		if err != nil {
			results = append(results, fail(name,
				fmt.Sprintf("command not found: %s", cmd),
				fmt.Sprintf("install the CLI or update command in catalog/providers/%s.yaml", id)))
			continue
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			results = append(results, fail(name,
				fmt.Sprintf("command not executable: %s", expanded),
				fmt.Sprintf("chmod +x %s", expanded)))
			continue
		}
		results = append(results, ok(name, expanded))
	}
	return results
}

// checkLogsDir verifies the logs directory exists and is writable.
func checkLogsDir(stateDir string) checkResult {
	logsDir := filepath.Join(stateDir, "logs")
	info, err := os.Stat(logsDir)
	if err != nil {
		return warn("logs-dir", fmt.Sprintf("not found: %s", logsDir),
			"run: tether init  or: mkdir -p "+logsDir)
	}
	if !info.IsDir() {
		return fail("logs-dir", fmt.Sprintf("not a directory: %s", logsDir),
			"remove the conflicting file: "+logsDir)
	}
	f, err := os.CreateTemp(logsDir, ".doctor-write-check.*")
	if err != nil {
		return warn("logs-dir", fmt.Sprintf("logs dir not writable: %s", logsDir),
			fmt.Sprintf("fix permissions: chmod u+w %s", logsDir))
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return ok("logs-dir", logsDir)
}

func checkIdentity(cat *config.Catalog) checkResult {
	if cat == nil {
		return warn("caller-identity", "skipped — catalog unavailable", "fix catalog first")
	}
	global := identity.Mode(cat.Global.Identity.EffectiveMode())
	if err := global.Validate(); err != nil {
		return fail("caller-identity", "invalid identity.mode", "use off, observe or enforce; validation happens at daemon start")
	}
	for point, m := range cat.Global.Identity.Points {
		if !slices.Contains(config.IdentityPoints, point) {
			return fail("caller-identity", "unknown identity.points entry "+point, "use one of "+strings.Join(config.IdentityPoints, ", "))
		}
		if identity.Mode(strings.ToLower(strings.TrimSpace(m))).Validate() != nil {
			return fail("caller-identity", "invalid identity.points."+point, "use off, observe or enforce")
		}
	}
	if _, _, err := verifyFlipLog(flipLogPath(config.Expand(catalogPath))); err != nil {
		return fail("caller-identity", "identity flip log integrity failure: "+err.Error(), "the audit chain is broken; treat flips as unverified, investigate before any further flip, restore the log from backup")
	}
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil {
		return warn("caller-identity", "daemon settings unavailable", "fix daemon settings")
	}
	if err := cfg.ValidateListenerBind(cat.Global.Daemon.MCPEndpoint.Enabled); err != nil {
		return warn("caller-identity", "daemon start would reject identity/listener settings", "use a local listener or enforce local_api (and mcp_gateway when its endpoint is enabled)")
	}
	st := loadIdentityModeState(cat, false)
	summary := "global=" + string(global)
	for _, p := range config.IdentityPoints {
		if _, set := cat.Global.Identity.Points[p]; set {
			summary += " " + p + "=" + st.Configured[p]
		}
	}
	if cfg.StrictestIdentityMode() == identity.Off {
		return ok("caller-identity", "off ("+summary+")")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := client.New(cfg.ListenAddr, client.WithToken("")).Health(ctx)
	if err != nil || h.Identity == nil {
		return warn("caller-identity", summary+" configured; runtime identity unavailable", "check daemon status")
	}
	st = loadIdentityModeState(cat, true)
	if pending := st.pendingRestart(); len(pending) > 0 {
		return warn("caller-identity", summary+"; restart pending for "+strings.Join(pending, ", ")+" (running modes differ from configured)", "restart the daemon to apply the configured identity modes")
	}
	if h.Identity.OperatorDegraded {
		return warn("caller-identity", "operator credentials degraded; observe daemon remains available", "restore matching DB/token backups or explicitly recover credentials before enforce")
	}
	if h.Identity.Audit.Dropped > 0 || h.Identity.Audit.Failures > 0 {
		return warn("caller-identity", fmt.Sprintf("audit dropped=%d failures=%d", h.Identity.Audit.Dropped, h.Identity.Audit.Failures), "check queue load and state DB availability")
	}
	return ok("caller-identity", summary+"; operator credentials available")
}

func checkMCPLiveNames(cat *config.Catalog, root string) []checkResult {
	adapter := mcpadapter.New(&app.Service{Catalog: cat}, "", nil)
	adapter.SetProtectedPaths(doctorProtect)
	opts := mcpadapter.ProxyOptions{}
	if value, present := os.LookupEnv("TETHER_MCP_SERVERS"); present {
		opts.ServerFilter = []string{}
		if value != "" {
			for _, id := range strings.Split(value, ",") {
				opts.ServerFilter = append(opts.ServerFilter, strings.TrimSpace(id))
			}
		}
	}
	status, err := adapter.ProbeNames(context.Background(), config.Expand(root), opts)
	var out []checkResult
	if err != nil {
		out = append(out, fail("mcp-live-names", err.Error(), "fix the named origin/tool or declare tool_prefix on one upstream"))
	}
	unexamined := map[string]int{}
	for _, finding := range status.Lint {
		if finding.Code == "conformance_unexamined" {
			unexamined[finding.Origin]++
			continue
		}
		remedy := "fix the upstream name or declared tool_prefix; names are not rewritten"
		switch finding.Code {
		case "missing_annotations", "description_length", "disabled_description", "input_schema":
			remedy = "fix the upstream tool declaration; metadata passes through unchanged"
		case "instructions_length":
			remedy = "shorten upstream initialization instructions to at most 2048 Unicode characters"
		case "input_schema_limit":
			remedy = "simplify the upstream schema to fit the bounded conformance check"
		case "input_schema_unexamined":
			remedy = "bundle referenced schemas locally so the offline resolver can check them"
		}
		message := finding.Message
		if finding.Name != "" {
			message = finding.Name + ": " + message
		}
		out = append(out, warn("mcp-name:"+finding.Origin+":"+finding.Code, message, remedy))
	}
	ids := make([]string, 0, len(unexamined))
	for id := range unexamined {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, warn("mcp-name:"+id+":conformance_unexamined", fmt.Sprintf("%d tool declarations unexamined: aggregate conformance budget exhausted", unexamined[id]), "request gateway status again to examine the next bounded batch; no missing findings are implied to be passes"))
	}
	for _, origin := range status.Origins {
		if origin.Status != "connected" {
			message := "upstream " + origin.Status + "; tools/list discovery incomplete"
			if !origin.InventoryExamined {
				message += "; inventory unexamined"
			}
			if origin.Error != "" {
				message += ": " + origin.Error
			}
			out = append(out, warn("mcp-live:"+origin.ID, message, "restore upstream availability or confinement configuration"))
		}
	}
	if len(out) == 0 {
		out = append(out, ok("mcp-live-names", "bounded live tools/list probe found no name or metadata issues"))
	}
	return out
}
