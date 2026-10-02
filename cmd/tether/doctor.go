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
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/mcptransport"
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
	doctorCmd.Flags().Bool("mcp-live", false, "spawn configured MCP upstreams for a bounded initialize/tools/list-only naming probe (no tool calls; may resolve helper credentials)")
	doctorCmd.Flags().StringArrayVar(&doctorProtect, "protect-path", nil, "protected paths for live MCP probe confinement (repeatable, same as tether mcp)")
}

// checkResult is the stable JSON shape for tether doctor --json.
type checkResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // "ok", "warn", "fail"
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
}

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

func ok(name, msg string) checkResult { return checkResult{Name: name, Status: statusOK, Message: msg} }
func warn(name, msg, remedy string) checkResult {
	return checkResult{Name: name, Status: statusWarn, Message: msg, Remedy: remedy}
}
func fail(name, msg, remedy string) checkResult {
	return checkResult{Name: name, Status: statusFail, Message: msg, Remedy: remedy}
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
	if cat != nil {
		checks = append(checks, checkSandboxProfiles(cat))
		checks = append(checks, checkMCPDiscoveryMode(cat), checkMCPEndpoint(cat))
		checks = append(checks, checkMCPProfiles(cat, catalogRoot)...)
		checks = append(checks, checkMCPNamingConfig(catalogRoot)...)
		if len(live) > 0 && live[0] {
			checks = append(checks, checkMCPLiveNames(cat, catalogRoot)...)
		}
		checks = append(checks, ok("events-retention", retentionMessage(cat.Global.Daemon.EventsRetention)))
	}
	checks = append(checks, doctorSandboxProtect(cat, catalogRoot)...)
	checks = append(checks, checkMCPCredentialFiles(catalogRoot)...)

	// 3. Daemon reachable (requires catalog for listen addr).
	checks = append(checks, checkDaemon(cat), checkIdentity(cat))
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

// checkSandboxProfiles fails when an agent names a sandbox profile the
// catalog does not define. The daemon still starts, but refuses every
// launch of that agent (CW-20261001-0130), so it is a failure here.
func checkSandboxProfiles(cat *config.Catalog) checkResult {
	issues := cat.SandboxIssues()
	if len(issues) == 0 {
		return ok("catalog-sandbox-profiles", fmt.Sprintf("every agent's sandbox profile is defined (%d profiles)", len(cat.SandboxProfiles)))
	}
	msgs := make([]string, len(issues))
	for i, issue := range issues {
		msgs[i] = issue.Error()
	}
	return fail("catalog-sandbox-profiles", strings.Join(msgs, "; "),
		"add the missing profile under sandbox-profiles/ or fix the agent's permissions.default_sandbox, then restart the daemon")
}

// checkCatalog loads and validates the catalog. Returns the loaded catalog (nil on failure).
func checkCatalog(catalogRoot string) (checkResult, *config.Catalog) {
	cat, err := config.LoadLayered(catalogRoot)
	if err != nil {
		return fail("catalog-present", fmt.Sprintf("load failed: %v", err),
			"run: tether init"), nil
	}
	if err := cat.ValidateMCPGrants(); err != nil {
		return fail("catalog-mcp-grants", err.Error(), "fix the named grant or enable its upstream under mcp-servers/"), nil
	}
	if err := cat.Validate(); err != nil {
		return fail("catalog-valid", fmt.Sprintf("validation failed: %v", err),
			"check your catalog YAML files or run: tether init --force"), nil
	}
	return ok("catalog-present", catalogRoot), cat
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
	mode := identity.Mode(cat.Global.Identity.EffectiveMode())
	if err := mode.Validate(); err != nil {
		return fail("caller-identity", "invalid identity.mode", "use off, observe or enforce; validation happens at daemon start")
	}
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil {
		return warn("caller-identity", "daemon settings unavailable", "fix daemon settings")
	}
	if err := identity.ValidateBind(cfg.ListenAddr, mode); err != nil {
		return warn("caller-identity", "daemon start would reject identity/listener settings", "use a local listener or explicitly configure enforce")
	}
	if mode == identity.Off {
		return ok("caller-identity", "off")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := client.New(cfg.ListenAddr, client.WithToken("")).Health(ctx)
	if err != nil || h.Identity == nil {
		return warn("caller-identity", string(mode)+" configured; runtime identity unavailable", "check daemon status")
	}
	if h.Identity.OperatorDegraded {
		return warn("caller-identity", "operator credentials degraded; observe daemon remains available", "restore matching DB/token backups or explicitly recover credentials before enforce")
	}
	if h.Identity.Audit.Dropped > 0 || h.Identity.Audit.Failures > 0 {
		return warn("caller-identity", fmt.Sprintf("audit dropped=%d failures=%d", h.Identity.Audit.Dropped, h.Identity.Audit.Failures), "check queue load and state DB availability")
	}
	return ok("caller-identity", string(h.Identity.Mode)+"; operator credentials available")
}

func checkMCPDiscoveryMode(cat *config.Catalog) checkResult {
	if _, err := mcpgateway.ResolveMode(mcpgateway.ModeInputs{Gateway: cat.Global.MCP.DiscoveryMode}); err != nil {
		return fail("mcp-discovery-mode", err.Error(), "fix mcp.discovery_mode and mcp.profiles fields in global.yaml")
	}
	return ok("mcp-discovery-mode", "configured discovery modes are valid")
}

// Doctor validates all authored profiles without connecting upstreams or reading
// credentials. Exact order/load names can only be verified against live tools.
func checkMCPProfiles(cat *config.Catalog, catalogRoot string) []checkResult {
	entries, err := config.LoadMCPServerCatalog(catalogRoot)
	if err != nil {
		return []checkResult{fail("mcp-profiles", err.Error(), "fix the MCP server catalog")}
	}
	known := map[string]bool{"tether": true}
	for _, entry := range entries {
		if entry.ID != "tether" {
			known[entry.ID] = entry.IsEnabled()
		}
	}
	var results []checkResult
	for id, profile := range cat.Global.MCP.Profiles {
		name := "mcp-profile-" + id
		if id == "" {
			results = append(results, fail(name, "empty profile ID", "name the profile"))
			continue
		}
		if err := profile.Validate(); err != nil {
			results = append(results, fail(name, err.Error(), "fix profile fields in global.yaml"))
			continue
		}
		if _, err := mcpgateway.SelectOrigins(known, nil, &profile); err != nil {
			results = append(results, fail(name, err.Error(), "use enabled catalog origins or reserved native tether"))
			continue
		}

		collision := false
		for _, id := range profile.Servers {
			if id == "tether" {
				for _, entry := range entries {
					if entry.ID == "tether" {
						collision = true
					}
				}
			}
		}
		if collision {
			results = append(results, fail(name, "reserved native tether origin conflicts with catalog upstream tether", "rename the upstream before selecting the reserved native origin"))
			continue
		}
		if len(profile.Order)+len(profile.AlwaysLoad) > 0 {
			results = append(results, warn(name, "order/always_load tool names require live upstream discovery", "start the selected gateway profile to validate exact names"))
		} else {
			results = append(results, ok(name, "profile syntax and origins are valid"))
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results
}

func checkMCPNamingConfig(root string) []checkResult {
	entries, err := config.LoadMCPServerCatalog(config.Expand(root))
	if err != nil {
		return []checkResult{fail("mcp-naming", "cannot read MCP server catalog", "fix catalog YAML")}
	}
	var out []checkResult
	ids := []string{}
	for _, entry := range entries {
		if entry.IsEnabled() {
			if entry.ID == "tether" {
				out = append(out, warn("mcp-origins", mcpgateway.ValidateOriginIDs([]string{"tether"}).Error(), "rename the upstream ID; startup fails only when this upstream is selected"))
				continue
			}
			ids = append(ids, entry.ID)
		}
	}
	if err := mcpgateway.ValidateOriginIDs(ids); err != nil {
		out = append(out, fail("mcp-origins", err.Error(), "fix enabled upstream IDs; tether is reserved"))
	}
	for _, entry := range entries {
		if entry.ToolPrefixInvalid {
			out = append(out, warn("mcp-prefix:"+entry.ID, entry.CatalogFile+": tool_prefix must be a string; invalid value ignored", "declare a string tool_prefix"))
		}
		if !entry.IsEnabled() {
			continue
		}
		if entry.ToolPrefix != "" {
			for _, finding := range mcpgateway.LintName(entry.ID, entry.ToolPrefix) {
				out = append(out, warn("mcp-prefix:"+entry.ID, finding.Message, "fix declared tool_prefix; live names require --mcp-live"))
			}
		}
	}
	if len(out) == 0 {
		out = append(out, ok("mcp-naming", "catalog origin/prefix declarations checked; live collision/name checks require --mcp-live"))
	}
	return out
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
	for _, finding := range status.Lint {
		out = append(out, warn("mcp-name:"+finding.Origin+":"+finding.Code, finding.Name+": "+finding.Message, "fix the upstream name or declared tool_prefix; names are not rewritten"))
	}
	for _, origin := range status.Origins {
		if origin.Status != "connected" {
			out = append(out, warn("mcp-live:"+origin.ID, "upstream "+origin.Status+"; tools/list discovery incomplete", "restore upstream availability or confinement configuration"))
		}
	}
	if len(out) == 0 {
		out = append(out, ok("mcp-live-names", "bounded live tools/list probe found no naming issues"))
	}
	return out
}

func checkMCPEndpoint(cat *config.Catalog) checkResult {
	if !cat.Global.Daemon.MCPEndpoint.Enabled {
		return ok("mcp-endpoint", "disabled (daemon.mcp_endpoint.enabled defaults false)")
	}
	addr := config.Expand(cat.Global.Daemon.ListenAddr)
	if err := mcptransport.ValidateEndpoint(addr, identity.Mode(cat.Global.Identity.Mode)); err != nil {
		return fail("mcp-endpoint", err.Error(), "fix daemon.listen_addr or disable daemon.mcp_endpoint.enabled")
	}
	return ok("mcp-endpoint", "enabled; strict verified admission on /mcp and /p/<profile>")
}
