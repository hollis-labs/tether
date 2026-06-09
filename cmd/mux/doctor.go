package main

// doctor.go — mux detect + mux doctor: provider detection table and health checks (T-v06x-01-04).
//
// mux detect: print provider detection results; --json for machine output. Exit 0 always.
// mux doctor: run ordered health checks and exit non-zero if any check is fail.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/setup"
	"github.com/hollis-labs/tether/internal/store"
)

// ── mux detect ───────────────────────────────────────────────────────────────

var detectCmd = &cobra.Command{
	Use:   "detect",
	Short: "Show which CLI providers (claude, codex, opencode) were found on this system",
	Long: `mux detect calls DetectProviders() and prints a table showing which CLI
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

// detectRow is the stable JSON shape for mux detect --json.
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

// ── mux doctor ───────────────────────────────────────────────────────────────

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check Tether install health: daemon, catalog, migrations, providers, and paths",
	Long: `mux doctor runs an ordered set of checks against your Tether installation
and reports ok / warn / fail for each one, with a one-line remedy on failures.

Exit code:
  0 — all checks ok or warn (warnings are advisory, not blocking)
  1 — one or more checks failed

--json prints a JSON array of check results for scripting.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOut, _ := cmd.Flags().GetBool("json")
		stateDir := filepath.Dir(config.Expand(catalogPath))
		return runDoctor(os.Stdout, stateDir, catalogPath, jsonOut) //nolint:wrapcheck
	},
}

func init() {
	doctorCmd.Flags().Bool("json", false, "print JSON array of check results")
}

// checkResult is the stable JSON shape for mux doctor --json.
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

func runDoctor(out io.Writer, stateDir, catalogRoot string, jsonOut bool) error {
	var checks []checkResult

	// 1. State dir exists + writable.
	checks = append(checks, checkStateDir(stateDir))

	// 2. Catalog present + valid.
	var cat *config.Catalog
	catalogCheck, loadedCat := checkCatalog(catalogRoot)
	checks = append(checks, catalogCheck)
	cat = loadedCat

	// 3. Daemon reachable (requires catalog for listen addr).
	checks = append(checks, checkDaemon(cat))

	// 4. Migrations current (opens DB; idempotent — migrations are a no-op if already applied).
	checks = append(checks, checkMigrations(cat))

	// 5. Provider commands resolvable.
	checks = append(checks, checkProviders(cat)...)

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
			"run: mux init")
	}
	if !info.IsDir() {
		return fail("state-dir", fmt.Sprintf("not a directory: %s", stateDir),
			"remove the conflicting file and run: mux init")
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
	cat, err := config.LoadLayered(catalogRoot)
	if err != nil {
		return fail("catalog-present", fmt.Sprintf("load failed: %v", err),
			"run: mux init"), nil
	}
	if err := cat.Validate(); err != nil {
		return fail("catalog-valid", fmt.Sprintf("validation failed: %v", err),
			"check your catalog YAML files or run: mux init --force"), nil
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
	c := client.New(cfg.ListenAddr)
	if err := c.Ping(ctx); err != nil {
		return warn("daemon-reachable", "daemon not running",
			"start it with: mux daemon start")
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
			"run: mux init  or check DB permissions at "+dbPath)
	}
	_ = db.Close()
	return ok("migrations-current", dbPath)
}

// checkProviders checks that each configured provider's command is resolvable.
func checkProviders(cat *config.Catalog) []checkResult {
	if cat == nil {
		return []checkResult{warn("providers", "skipped — catalog unavailable", "fix catalog first")}
	}
	if len(cat.Providers) == 0 {
		return []checkResult{warn("providers", "no providers configured",
			"run: mux init  to seed provider catalog")}
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
			brand := p.Provider // e.g. "claude", "opencode"
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
			"run: mux init  or: mkdir -p "+logsDir)
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
